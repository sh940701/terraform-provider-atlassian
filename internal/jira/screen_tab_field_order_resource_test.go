package jira_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

// tabOrderServer — 탭 하나의 칸 순서를 흉내 낸다. move 호출(First / after)을 실제 순서에 반영한다.
type tabOrderServer struct {
	mu    sync.Mutex
	order []string
	moves []string
}

func (s *tabOrderServer) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.Method == "GET" && isFieldsListPath(r.URL.Path):
		out := make([]map[string]interface{}, 0, len(s.order))
		for _, id := range s.order {
			out = append(out, fieldJSON(id, strings.ToUpper(id)))
		}
		json.NewEncoder(w).Encode(out) //nolint:errcheck
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/move") && isFieldPath(strings.TrimSuffix(r.URL.Path, "/move")):
		parts := strings.Split(strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, "/move"), "/rest/api/3/screens/"), "/")
		id := parts[4]
		var body struct {
			After    string `json:"after"`
			Position string `json:"position"`
		}
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		s.moves = append(s.moves, id+"→"+body.After+body.Position)
		rest := make([]string, 0, len(s.order))
		for _, x := range s.order {
			if x != id {
				rest = append(rest, x)
			}
		}
		switch {
		case body.Position == "First":
			s.order = append([]string{id}, rest...)
		case body.After != "":
			s.order = s.order[:0]
			for _, x := range rest {
				s.order = append(s.order, x)
				if x == body.After {
					s.order = append(s.order, id)
				}
			}
		default:
			s.order = append(rest, id)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func TestAccScreenTabFieldOrderResource_ordersListedFieldsFirst(t *testing.T) {
	srv := &tabOrderServer{order: []string{"a", "b", "c", "d"}}
	mockServer := httptest.NewServer(http.HandlerFunc(srv.handler))
	defer mockServer.Close()
	t.Setenv("ATLASSIAN_URL", mockServer.URL)
	t.Setenv("ATLASSIAN_USER", "test@test.com")
	t.Setenv("ATLASSIAN_TOKEN", "test-token")

	config := func(ids string) string {
		return fmt.Sprintf(`
resource "atlassian_jira_screen_tab_field_order" "o" {
  screen_id = "10036"
  tab_id    = "10047"
  field_ids = [%s]
}`, ids)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config(`"b", "a", "c"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_screen_tab_field_order.o", "field_ids.#", "3"),
					resource.TestCheckResourceAttr("atlassian_jira_screen_tab_field_order.o", "field_ids.0", "b"),
					resource.TestCheckResourceAttr("atlassian_jira_screen_tab_field_order.o", "field_ids.2", "c"),
					func(_ *terraform.State) error {
						srv.mu.Lock()
						defer srv.mu.Unlock()
						if got := strings.Join(srv.order, ","); got != "b,a,c,d" {
							return fmt.Errorf("tab order after create = %s, want b,a,c,d", got)
						}
						if got := strings.Join(srv.moves, " "); got != "b→First a→b c→a" {
							return fmt.Errorf("moves = %q", got)
						}
						return nil
					},
				),
			},
			{
				// 순서를 바꾸면 제자리 갱신 — 나열 안 한 d 는 뒤에 남는다
				Config: config(`"c", "b", "a"`),
				Check: func(_ *terraform.State) error {
					srv.mu.Lock()
					defer srv.mu.Unlock()
					if got := strings.Join(srv.order, ","); got != "c,b,a,d" {
						return fmt.Errorf("tab order after update = %s, want c,b,a,d", got)
					}
					return nil
				},
			},
			{
				// 밖에서 순서가 바뀌면 plan 이 잡는다 — 현재 순서로 state 를 읽고 다음 apply 가 되돌린다
				PreConfig: func() {
					srv.mu.Lock()
					srv.order = []string{"a", "c", "b", "d"}
					srv.mu.Unlock()
				},
				Config: config(`"c", "b", "a"`),
				Check: func(_ *terraform.State) error {
					srv.mu.Lock()
					defer srv.mu.Unlock()
					if got := strings.Join(srv.order, ","); got != "c,b,a,d" {
						return fmt.Errorf("tab order after drift repair = %s, want c,b,a,d", got)
					}
					return nil
				},
			},
			{
				// Import takes the tab's whole current order (c,b,a,d) — one more than the config listed.
				ResourceName:            "atlassian_jira_screen_tab_field_order.o",
				ImportState:             true,
				ImportStateId:           "10036/10047",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"field_ids"},
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("imported %d states, want 1", len(states))
					}
					a := states[0].Attributes
					got := strings.Join([]string{a["field_ids.0"], a["field_ids.1"], a["field_ids.2"], a["field_ids.3"]}, ",")
					if a["field_ids.#"] != "4" || got != "c,b,a,d" {
						return fmt.Errorf("imported field_ids = %s (%s), want c,b,a,d", got, a["field_ids.#"])
					}
					return nil
				},
			},
		},
	})
}

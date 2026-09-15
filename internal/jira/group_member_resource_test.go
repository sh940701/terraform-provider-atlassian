package jira_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

const (
	gmGroupID   = "00000000-0000-0000-0000-00000000abcd"
	gmAccountID = "5b10ac8d82e05b22cc7d4ef5"
)

// groupMemberMock models Jira's group membership endpoints:
//
//	POST   /rest/api/3/group/user?groupId=      {accountId}  → 201
//	GET    /rest/api/3/group/member?groupId=&includeInactiveUsers=true&startAt=&maxResults=  (paginated)
//	DELETE /rest/api/3/group/user?groupId=&accountId=         → 200
type groupMemberMock struct {
	mu      sync.Mutex
	members map[string][]string // groupId → accountIds
	// pageSize < len(members) forces pagination on GET.
	pageSize int
}

func (m *groupMemberMock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.Method == "POST" && r.URL.Path == "/rest/api/3/group/user":
			var body struct {
				AccountID string `json:"accountId"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.AccountID == "" || q.Get("groupId") == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			m.mu.Lock()
			m.members[q.Get("groupId")] = append(m.members[q.Get("groupId")], body.AccountID)
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"groupId": q.Get("groupId"), "name": "tf-test-group"})

		case r.Method == "GET" && r.URL.Path == "/rest/api/3/group/member":
			if q.Get("includeInactiveUsers") != "true" {
				w.WriteHeader(http.StatusBadRequest) // the resource must ask for inactive users
				return
			}
			m.mu.Lock()
			all, ok := m.members[q.Get("groupId")]
			m.mu.Unlock()
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			startAt, maxResults := 0, 50
			fmt.Sscanf(q.Get("startAt"), "%d", &startAt)
			fmt.Sscanf(q.Get("maxResults"), "%d", &maxResults)
			if m.pageSize > 0 && maxResults > m.pageSize {
				maxResults = m.pageSize
			}
			end := startAt + maxResults
			if end > len(all) {
				end = len(all)
			}
			values := []map[string]interface{}{}
			for _, id := range all[startAt:end] {
				values = append(values, map[string]interface{}{"accountId": id, "active": true, "displayName": "u " + id})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"startAt": startAt, "maxResults": maxResults, "total": len(all), "isLast": end >= len(all), "values": values,
			})

		case r.Method == "DELETE" && r.URL.Path == "/rest/api/3/group/user":
			m.mu.Lock()
			defer m.mu.Unlock()
			list := m.members[q.Get("groupId")]
			for i, id := range list {
				if id == q.Get("accountId") {
					m.members[q.Get("groupId")] = append(list[:i], list[i+1:]...)
					w.WriteHeader(http.StatusOK)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (m *groupMemberMock) has(groupID, accountID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.members[groupID] {
		if id == accountID {
			return true
		}
	}
	return false
}

func setupGroupMemberMock(t *testing.T, mock *groupMemberMock) {
	t.Helper()
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)
	t.Setenv("ATLASSIAN_URL", srv.URL)
	t.Setenv("ATLASSIAN_USER", "test@test.com")
	t.Setenv("ATLASSIAN_TOKEN", "test-token")
}

func TestAccGroupMemberResource_basic(t *testing.T) {
	mock := &groupMemberMock{members: map[string][]string{gmGroupID: {}}}
	setupGroupMemberMock(t, mock)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		CheckDestroy: func(_ *terraform.State) error {
			if mock.has(gmGroupID, gmAccountID) {
				return fmt.Errorf("member %s still in group %s after destroy", gmAccountID, gmGroupID)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`resource "atlassian_jira_group_member" "test" {
  group_id   = %q
  account_id = %q
}`, gmGroupID, gmAccountID),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_group_member.test", "id", gmGroupID+"/"+gmAccountID),
					resource.TestCheckResourceAttr("atlassian_jira_group_member.test", "group_id", gmGroupID),
					resource.TestCheckResourceAttr("atlassian_jira_group_member.test", "account_id", gmAccountID),
					func(_ *terraform.State) error {
						if !mock.has(gmGroupID, gmAccountID) {
							return fmt.Errorf("member not added on the server")
						}
						return nil
					},
				),
			},
			{
				ResourceName:      "atlassian_jira_group_member.test",
				ImportState:       true,
				ImportStateId:     gmGroupID + "/" + gmAccountID,
				ImportStateVerify: true,
			},
		},
	})
}

func TestAccGroupMemberResource_ReadPaginatesAndRemovesWhenGone(t *testing.T) {
	// 60 members before ours, page size 25 → our membership is on page 3.
	pre := make([]string, 0, 60)
	for i := 0; i < 60; i++ {
		pre = append(pre, fmt.Sprintf("pre%02d", i))
	}
	mock := &groupMemberMock{members: map[string][]string{gmGroupID: pre}, pageSize: 25}
	setupGroupMemberMock(t, mock)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`resource "atlassian_jira_group_member" "test" {
  group_id   = %q
  account_id = %q
}`, gmGroupID, gmAccountID),
				Check: resource.TestCheckResourceAttr("atlassian_jira_group_member.test", "id", gmGroupID+"/"+gmAccountID),
			},
			{
				// Out-of-band removal → Read must drop the resource so the plan re-adds it.
				PreConfig: func() {
					mock.mu.Lock()
					mock.members[gmGroupID] = pre
					mock.mu.Unlock()
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

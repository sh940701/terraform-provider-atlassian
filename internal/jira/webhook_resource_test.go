package jira_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

// webhookMock models the administrator webhook API (/rest/webhooks/1.0/webhook).
// It mirrors the documented semantics that matter to the resource:
//   - POST returns the new webhook with `self` (the id is its last path segment)
//   - GET never returns `secret`; `isSigned` tells whether one is set
//   - PUT with `secret` omitted keeps it; "" removes it
type webhookMock struct {
	mu     sync.Mutex
	nextID int
	hooks  map[string]map[string]interface{} // id → stored document (incl. secret)
}

func newWebhookMock() *webhookMock {
	return &webhookMock{nextID: 41, hooks: map[string]map[string]interface{}{}}
}

func (m *webhookMock) view(baseURL, id string) map[string]interface{} {
	h := m.hooks[id]
	secret, _ := h["secret"].(string)
	return map[string]interface{}{
		"name":        h["name"],
		"description": h["description"],
		"url":         h["url"],
		"excludeBody": h["excludeBody"],
		"events":      h["events"],
		"filters":     h["filters"],
		"enabled":     h["enabled"],
		"self":        baseURL + "/rest/webhooks/1.0/webhook/" + id,
		"lastUpdated": 1789289114723,
		"isSigned":    secret != "",
	}
}

func (m *webhookMock) handler() http.HandlerFunc {
	idRe := regexp.MustCompile(`^/rest/webhooks/1\.0/webhook/(\d+)$`)
	return func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		m.mu.Lock()
		defer m.mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/rest/webhooks/1.0/webhook":
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["name"] == "" || body["url"] == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if u, _ := body["url"].(string); !strings.HasPrefix(u, "https://") {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"errorMessages":["Only secure HTTPS URLs are allowed."]}`))
				return
			}
			id := fmt.Sprint(m.nextID)
			m.nextID++
			if _, ok := body["enabled"]; !ok {
				body["enabled"] = true
			}
			if _, ok := body["description"]; !ok {
				body["description"] = ""
			}
			m.hooks[id] = body
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(m.view(base, id))

		case r.Method == "GET" && idRe.MatchString(r.URL.Path):
			id := idRe.FindStringSubmatch(r.URL.Path)[1]
			if _, ok := m.hooks[id]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(m.view(base, id))

		case r.Method == "PUT" && idRe.MatchString(r.URL.Path):
			id := idRe.FindStringSubmatch(r.URL.Path)[1]
			cur, ok := m.hooks[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			for k, v := range body {
				if k == "secret" {
					if s, _ := v.(string); s == "" || v == nil {
						delete(cur, "secret")
					} else {
						cur["secret"] = s
					}
					continue
				}
				cur[k] = v
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(m.view(base, id))

		case r.Method == "DELETE" && idRe.MatchString(r.URL.Path):
			id := idRe.FindStringSubmatch(r.URL.Path)[1]
			if _, ok := m.hooks[id]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			delete(m.hooks, id)
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (m *webhookMock) get(id string) (map[string]interface{}, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.hooks[id]
	return h, ok
}

func setupWebhookMock(t *testing.T, mock *webhookMock) {
	t.Helper()
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)
	t.Setenv("ATLASSIAN_URL", srv.URL)
	t.Setenv("ATLASSIAN_USER", "test@test.com")
	t.Setenv("ATLASSIAN_TOKEN", "test-token")
}

const webhookConfig = `resource "atlassian_jira_webhook" "test" {
  name         = "K-CARE collector"
  url          = "https://example.com/api/webhook/jira"
  events       = ["jira:issue_created", "jira:issue_updated", "comment_created"]
  jql          = "project = DEV"
  exclude_body = false
  secret       = %q
}`

func TestAccWebhookResource_basic(t *testing.T) {
	mock := newWebhookMock()
	setupWebhookMock(t, mock)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		CheckDestroy: func(_ *terraform.State) error {
			if _, ok := mock.get("41"); ok {
				return fmt.Errorf("webhook 41 still exists after destroy")
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(webhookConfig, "s3cr3t"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_webhook.test", "id", "41"),
					resource.TestCheckResourceAttr("atlassian_jira_webhook.test", "name", "K-CARE collector"),
					resource.TestCheckResourceAttr("atlassian_jira_webhook.test", "jql", "project = DEV"),
					resource.TestCheckResourceAttr("atlassian_jira_webhook.test", "events.#", "3"),
					resource.TestCheckResourceAttr("atlassian_jira_webhook.test", "enabled", "true"),
					resource.TestCheckResourceAttr("atlassian_jira_webhook.test", "is_signed", "true"),
					resource.TestCheckResourceAttr("atlassian_jira_webhook.test", "secret", "s3cr3t"),
					func(_ *terraform.State) error {
						h, _ := mock.get("41")
						if h["secret"] != "s3cr3t" {
							return fmt.Errorf("secret not sent on create: %v", h["secret"])
						}
						if f, _ := h["filters"].(map[string]interface{}); f["issue-related-events-section"] != "project = DEV" {
							return fmt.Errorf("jql not sent as filters: %v", h["filters"])
						}
						return nil
					},
				),
			},
			{
				// Rename only: PUT must NOT carry `secret` (omitted = keep).
				Config: strings.Replace(fmt.Sprintf(webhookConfig, "s3cr3t"), "K-CARE collector", "K-CARE collector v2", 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_webhook.test", "name", "K-CARE collector v2"),
					resource.TestCheckResourceAttr("atlassian_jira_webhook.test", "is_signed", "true"),
					func(_ *terraform.State) error {
						h, _ := mock.get("41")
						if h["secret"] != "s3cr3t" {
							return fmt.Errorf("secret must be kept when unchanged, got %v", h["secret"])
						}
						return nil
					},
				),
			},
			{
				// Removing the secret sends "" → server drops it → is_signed false.
				Config: fmt.Sprintf(webhookConfig, ""),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_webhook.test", "is_signed", "false"),
					func(_ *terraform.State) error {
						h, _ := mock.get("41")
						if _, ok := h["secret"]; ok {
							return fmt.Errorf("secret should have been removed")
						}
						return nil
					},
				),
			},
			{
				ResourceName:            "atlassian_jira_webhook.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"secret"}, // never returned by the API
			},
		},
	})
}

func TestAccWebhookResource_RejectsHttpAndBadPort(t *testing.T) {
	mock := newWebhookMock()
	setupWebhookMock(t, mock)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `resource "atlassian_jira_webhook" "test" {
  name   = "bad"
  url    = "http://example.com/hook"
  events = ["jira:issue_created"]
}`,
				ExpectError: regexp.MustCompile(`https://`),
			},
			{
				Config: `resource "atlassian_jira_webhook" "test" {
  name   = "bad"
  url    = "https://example.com:9999/hook"
  events = ["jira:issue_created"]
}`,
				ExpectError: regexp.MustCompile(`port`),
			},
		},
	})
}

func TestAccWebhookResource_DriftWhenDisabledOrDeleted(t *testing.T) {
	mock := newWebhookMock()
	setupWebhookMock(t, mock)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(webhookConfig, "s3cr3t"),
				Check:  resource.TestCheckResourceAttr("atlassian_jira_webhook.test", "enabled", "true"),
			},
			{
				// Disabled in the UI → Read sees enabled=false → plan wants to re-enable.
				PreConfig: func() {
					mock.mu.Lock()
					mock.hooks["41"]["enabled"] = false
					mock.mu.Unlock()
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
			},
			{
				// Deleted out-of-band → Read removes the resource → plan recreates.
				PreConfig: func() {
					mock.mu.Lock()
					delete(mock.hooks, "41")
					mock.mu.Unlock()
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

package jira_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

// automationRuleMock models the two Automation Rule Management API calls
// this resource makes (POST /rule, GET /rule/{ruleUuid}) plus the
// /_edge/tenant_info lookup the client uses to resolve the cloud ID that
// AutomationURL needs. All three are served from the same httptest server,
// so it doubles as both the Jira site and the Automation API base — origins
// match trivially, which is fine for what this test exercises.
type automationRuleMock struct {
	mu            sync.Mutex
	cloudID       string
	nextSeq       int
	rules         map[string]map[string]interface{} // uuid -> stored rule document (as sent, plus "uuid")
	posted        []map[string]interface{}          // raw POST bodies (rule object) in arrival order, un-augmented
	omitUUIDOnGet bool                              // simulates a GET response that (contrary to what's normally observed) omits "uuid"
}

func newAutomationRuleMock() *automationRuleMock {
	return &automationRuleMock{cloudID: "abc", nextSeq: 1, rules: map[string]map[string]interface{}{}}
}

// lastPosted returns the most recent raw POST body (the "rule" object exactly as
// the provider sent it, before the mock adds "uuid"), or nil if none arrived.
func (m *automationRuleMock) lastPosted() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.posted) == 0 {
		return nil
	}
	return m.posted[len(m.posted)-1]
}

func (m *automationRuleMock) handler() http.HandlerFunc {
	ruleIDRe := regexp.MustCompile(`^/abc/rest/v1/rule/([^/]+)$`)
	return func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := r.BasicAuth(); !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		m.mu.Lock()
		defer m.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/_edge/tenant_info":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"cloudId": m.cloudID})

		case r.Method == http.MethodPost && r.URL.Path == "/abc/rest/v1/rule":
			var body struct {
				Rule map[string]interface{} `json:"rule"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Rule == nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if name, _ := body.Rule["name"].(string); name == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			m.posted = append(m.posted, body.Rule)
			uuid := fmt.Sprintf("rule-uuid-%d", m.nextSeq)
			m.nextSeq++
			doc := make(map[string]interface{}, len(body.Rule)+1)
			for k, v := range body.Rule {
				doc[k] = v
			}
			doc["uuid"] = uuid
			m.rules[uuid] = doc
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(doc)

		case r.Method == http.MethodGet && ruleIDRe.MatchString(r.URL.Path):
			id := ruleIDRe.FindStringSubmatch(r.URL.Path)[1]
			doc, ok := m.rules[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			resp := doc
			if m.omitUUIDOnGet {
				resp = make(map[string]interface{}, len(doc))
				for k, v := range doc {
					if k != "uuid" {
						resp[k] = v
					}
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)

		case r.Method == http.MethodDelete && ruleIDRe.MatchString(r.URL.Path):
			id := ruleIDRe.FindStringSubmatch(r.URL.Path)[1]
			if _, ok := m.rules[id]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			delete(m.rules, id)
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (m *automationRuleMock) only() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, doc := range m.rules {
		return doc
	}
	return nil
}

func (m *automationRuleMock) setOmitUUIDOnGet(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.omitUUIDOnGet = v
}

// setupAutomationRuleMock starts the mock server and returns its URL. Unlike
// other resource tests, ATLASSIAN_URL is deliberately NOT set here: `url`
// and `automation_base_url` are supplied explicitly in each test's provider
// block instead (see automationRuleConfig), because they are per-test
// httptest addresses.
func setupAutomationRuleMock(t *testing.T, mock *automationRuleMock) string {
	t.Helper()
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)
	t.Setenv("ATLASSIAN_USER", "test@test.com")
	t.Setenv("ATLASSIAN_TOKEN", "test-token")
	return srv.URL
}

func automationRuleConfig(serverURL, bodyExpr string) string {
	return fmt.Sprintf(`provider "atlassian" {
  url                 = %[1]q
  automation_base_url = %[1]q
}

resource "atlassian_jira_automation_rule" "test" {
  name        = "K-CARE ticket watcher"
  project_ids = ["10549"]
  body        = %[2]s
}
`, serverURL, bodyExpr)
}

// bodyRawJSON is the `body` config value used throughout the test. Its keys
// are deliberately NOT in alphabetical order ("type" before "component",
// "value" before "type" before "component"): automationRuleMock stores the
// decoded rule as a map[string]interface{} and echoes it back on GET, and
// both Go's encoding/json and Terraform's jsonencode() marshal object keys
// alphabetically — so a mock (or a real API) that reformats/reorders JSON
// on the way back is exercised here for real, rather than by coincidence
// matching the config's order.
const bodyRawJSON = `<<-EOT
    {"trigger":{"type":"jira.manual.trigger.trigger","component":"TRIGGER"},"components":[{"value":{"assignee":"current-user"},"type":"jira.issue.assign","component":"ACTION"}]}
    EOT
`

func TestAccAutomationRuleResource_basic(t *testing.T) {
	mock := newAutomationRuleMock()
	serverURL := setupAutomationRuleMock(t, mock)

	resource.Test(t, resource.TestCase{
		// This test talks only to the local httptest server above, never a
		// real Atlassian site — IsUnitTest lets it run under plain
		// `go test` (and therefore CI's ci.yml, which does not set TF_ACC)
		// instead of being silently skipped like a real TF_ACC=1 acceptance
		// test would be.
		IsUnitTest:               true,
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Create: project_ids/body must reach the API as
				// ruleScopeARIs/trigger+components, state defaults to
				// ENABLED, and the response's uuid lands in state.
				Config: automationRuleConfig(serverURL, bodyRawJSON),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_automation_rule.test", "name", "K-CARE ticket watcher"),
					resource.TestCheckResourceAttr("atlassian_jira_automation_rule.test", "state", "ENABLED"),
					resource.TestCheckResourceAttrSet("atlassian_jira_automation_rule.test", "uuid"),
					resource.TestCheckResourceAttrPair(
						"atlassian_jira_automation_rule.test", "id",
						"atlassian_jira_automation_rule.test", "uuid",
					),
					func(_ *terraform.State) error {
						doc := mock.only()
						if doc == nil {
							return fmt.Errorf("mock received no rule")
						}
						if doc["name"] != "K-CARE ticket watcher" {
							return fmt.Errorf("unexpected name sent: %v", doc["name"])
						}
						if doc["state"] != "ENABLED" {
							return fmt.Errorf("unexpected state sent: %v", doc["state"])
						}
						aris, _ := doc["ruleScopeARIs"].([]interface{})
						if len(aris) != 1 || aris[0] != "ari:cloud:jira:abc:project/10549" {
							return fmt.Errorf("unexpected ruleScopeARIs sent: %v", doc["ruleScopeARIs"])
						}
						raw := mock.lastPosted()
						if raw == nil {
							return fmt.Errorf("mock recorded no POST body")
						}
						if v, ok := raw["uuid"]; ok {
							return fmt.Errorf("uuid must not be sent on create, got %v", v)
						}
						if v, ok := raw["actor"]; ok {
							return fmt.Errorf("actor must not be sent when actor_account_id is unset, got %v", v)
						}
						if !jsonEquivalent(doc["trigger"], map[string]interface{}{
							"component": "TRIGGER",
							"type":      "jira.manual.trigger.trigger",
						}) {
							return fmt.Errorf("unexpected trigger sent: %v", doc["trigger"])
						}
						wantComponents := []interface{}{
							map[string]interface{}{
								"component": "ACTION",
								"type":      "jira.issue.assign",
								"value":     map[string]interface{}{"assignee": "current-user"},
							},
						}
						if !jsonEquivalent(doc["components"], wantComponents) {
							return fmt.Errorf("unexpected components sent: %v", doc["components"])
						}
						return nil
					},
				),
			},
			{
				// Re-apply the identical config: Read must map the GET
				// document (whose trigger/components come back with keys in
				// a different order — see bodyRawJSON's comment) back onto
				// state with no drift. Without jsontypes.Normalized on
				// `body`, this step fails with a non-empty plan.
				Config:             automationRuleConfig(serverURL, bodyRawJSON),
				PlanOnly:           true,
				ExpectNonEmptyPlan: false,
			},
		},
	})
}

// TestAccAutomationRuleResource_ReadKeepsIdentifierWhenGetOmitsUUID covers a
// reviewer finding: Read must not blindly overwrite `uuid`/`id` from the GET
// document. If a response ever omitted "uuid" (contrary to what's normally
// observed — GET is expected to echo it back), state must keep the prior
// identifier rather than losing it, since a lost id sends every later
// Read/Update/Delete to "/rule/" instead of failing loudly or disappearing
// cleanly.
func TestAccAutomationRuleResource_ReadKeepsIdentifierWhenGetOmitsUUID(t *testing.T) {
	mock := newAutomationRuleMock()
	serverURL := setupAutomationRuleMock(t, mock)

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: automationRuleConfig(serverURL, bodyRawJSON),
				Check:  resource.TestCheckResourceAttr("atlassian_jira_automation_rule.test", "uuid", "rule-uuid-1"),
			},
			{
				// From here on, GET omits "uuid". Nothing else about the
				// rule changed, so if Read keeps the prior uuid/id (the fix
				// under test), the refresh plan is empty; if it instead
				// wipes them to "", the plan is non-empty (id is Computed
				// with UseStateForUnknown, so an unexpected "" would surface
				// as a would-recreate diff) and this step fails.
				PreConfig: func() {
					mock.setOmitUUIDOnGet(true)
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: false,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_automation_rule.test", "uuid", "rule-uuid-1"),
					resource.TestCheckResourceAttr("atlassian_jira_automation_rule.test", "id", "rule-uuid-1"),
				),
			},
		},
	})
}

// jsonEquivalent compares got (already interface{}-shaped, from decoding
// the mock's stored request body) and want (a literal Go value) as parsed
// JSON, ignoring key order and Go type differences (e.g. json.Number vs
// float64) that don't reflect a real difference.
func jsonEquivalent(got, want interface{}) bool {
	a, errA := json.Marshal(got)
	b, errB := json.Marshal(want)
	if errA != nil || errB != nil {
		return false
	}
	var ai, bi interface{}
	if err := json.Unmarshal(a, &ai); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bi); err != nil {
		return false
	}
	return reflect.DeepEqual(ai, bi)
}

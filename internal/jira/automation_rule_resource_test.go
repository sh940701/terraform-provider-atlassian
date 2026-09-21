package jira_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/testutil"
)

// scopeCall records one PUT .../rule-scope call.
type scopeCall struct {
	uuid string
	aris []string
}

// stateCall records one PUT .../state call.
type stateCall struct {
	uuid  string
	state string
}

// automationRuleMock models the Automation Rule Management API calls this
// resource makes — POST /rule, GET /rule/{ruleUuid}, PUT /rule/{ruleUuid},
// PUT /rule/{ruleUuid}/rule-scope, PUT /rule/{ruleUuid}/state, DELETE
// /rule/{ruleUuid} — plus the /_edge/tenant_info lookup the client uses to
// resolve the cloud ID that AutomationURL needs. All are served from the
// same httptest server, so it doubles as both the Jira site and the
// Automation API base — origins match trivially, which is fine for what
// this test exercises.
type automationRuleMock struct {
	mu                     sync.Mutex
	cloudID                string
	nextSeq                int
	rules                  map[string]map[string]interface{} // uuid -> stored rule document (as sent, plus "uuid")
	posted                 []map[string]interface{}          // raw POST bodies (rule object) in arrival order, un-augmented
	putRules               []map[string]interface{}          // raw PUT /rule/{uuid} bodies (rule object) in arrival order
	scopeCalls             []scopeCall                       // PUT .../rule-scope calls in arrival order
	stateCalls             []stateCall                       // PUT .../state calls in arrival order
	calls                  []string                          // every mutating call, in arrival order, for ordering assertions (e.g. disable-then-delete)
	omitUUIDOnGet          bool                              // simulates a GET response that (contrary to what's normally observed) omits "uuid"
	injectExtraScopeARI    string                            // if set, appended to ruleScopeARIs on create — simulates a non-project scope the server already knows about
	injectServerNoiseOnGet bool                              // if set, GET adds id/schemaVersion/empty conditions to trigger/components — simulates server echo noise
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

// lastPutRule returns the most recent raw PUT /rule/{uuid} body (the "rule"
// object exactly as the provider sent it), or nil if none arrived.
func (m *automationRuleMock) lastPutRule() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.putRules) == 0 {
		return nil
	}
	return m.putRules[len(m.putRules)-1]
}

// lastScopeCall returns the most recent PUT .../rule-scope call, or nil.
func (m *automationRuleMock) lastScopeCall() *scopeCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.scopeCalls) == 0 {
		return nil
	}
	c := m.scopeCalls[len(m.scopeCalls)-1]
	return &c
}

// lastStateCall returns the most recent PUT .../state call, or nil.
func (m *automationRuleMock) lastStateCall() *stateCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.stateCalls) == 0 {
		return nil
	}
	c := m.stateCalls[len(m.stateCalls)-1]
	return &c
}

// callIndex returns the (0-based) index of the first call in arrival order
// whose logged description equals want, or -1 if it never happened.
func (m *automationRuleMock) callIndex(want string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range m.calls {
		if c == want {
			return i
		}
	}
	return -1
}

func (m *automationRuleMock) handler() http.HandlerFunc {
	ruleIDRe := regexp.MustCompile(`^/abc/rest/v1/rule/([^/]+)$`)
	ruleScopeRe := regexp.MustCompile(`^/abc/rest/v1/rule/([^/]+)/rule-scope$`)
	ruleStateRe := regexp.MustCompile(`^/abc/rest/v1/rule/([^/]+)/state$`)
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
			if m.injectExtraScopeARI != "" {
				if aris, ok := doc["ruleScopeARIs"].([]interface{}); ok {
					doc["ruleScopeARIs"] = append(aris, m.injectExtraScopeARI)
				}
			}
			m.rules[uuid] = doc
			m.calls = append(m.calls, "POST rule "+uuid)
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
				stripped := make(map[string]interface{}, len(doc))
				for k, v := range doc {
					if k != "uuid" {
						stripped[k] = v
					}
				}
				resp = stripped
			}
			if m.injectServerNoiseOnGet {
				resp = injectAutomationRuleNoise(resp)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)

		case r.Method == http.MethodPut && ruleScopeRe.MatchString(r.URL.Path):
			id := ruleScopeRe.FindStringSubmatch(r.URL.Path)[1]
			doc, ok := m.rules[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var body struct {
				RuleScopeARIs []string `json:"ruleScopeARIs"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			m.scopeCalls = append(m.scopeCalls, scopeCall{uuid: id, aris: body.RuleScopeARIs})
			m.calls = append(m.calls, "PUT scope "+id)
			aris := make([]interface{}, len(body.RuleScopeARIs))
			for i, a := range body.RuleScopeARIs {
				aris[i] = a
			}
			doc["ruleScopeARIs"] = aris
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPut && ruleStateRe.MatchString(r.URL.Path):
			id := ruleStateRe.FindStringSubmatch(r.URL.Path)[1]
			doc, ok := m.rules[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var body struct {
				State string `json:"state"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.State == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			m.stateCalls = append(m.stateCalls, stateCall{uuid: id, state: body.State})
			m.calls = append(m.calls, "PUT state "+id+" "+body.State)
			doc["state"] = body.State
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPut && ruleIDRe.MatchString(r.URL.Path):
			id := ruleIDRe.FindStringSubmatch(r.URL.Path)[1]
			if _, ok := m.rules[id]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var body struct {
				Rule map[string]interface{} `json:"rule"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Rule == nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if v, ok := body.Rule["uuid"]; !ok || v != id {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			m.putRules = append(m.putRules, body.Rule)
			m.calls = append(m.calls, "PUT rule "+id)
			doc := make(map[string]interface{}, len(body.Rule))
			for k, v := range body.Rule {
				doc[k] = v
			}
			m.rules[id] = doc
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(doc)

		case r.Method == http.MethodDelete && ruleIDRe.MatchString(r.URL.Path):
			id := ruleIDRe.FindStringSubmatch(r.URL.Path)[1]
			doc, ok := m.rules[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			// The real API is documented (T6) to only delete a disabled
			// rule — enforced here so Delete's disable-then-delete order is
			// exercised for real rather than assumed.
			if s, _ := doc["state"].(string); s != "DISABLED" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			m.calls = append(m.calls, "DELETE "+id)
			delete(m.rules, id)
			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// injectAutomationRuleNoise returns a copy of doc with server-added noise on
// the rule itself and on trigger/components: an "id" and "schemaVersion" at
// the top level and on trigger, and an "id", "schemaVersion", and empty
// "conditions" on every component — the kind of echo normalizeBody is meant
// to see through.
func injectAutomationRuleNoise(doc map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(doc)+2)
	for k, v := range doc {
		out[k] = v
	}
	out["id"] = "rule-server-id"
	out["schemaVersion"] = 1
	if trigger, ok := out["trigger"].(map[string]interface{}); ok {
		t2 := make(map[string]interface{}, len(trigger)+2)
		for k, v := range trigger {
			t2[k] = v
		}
		t2["id"] = "trigger-server-id"
		t2["schemaVersion"] = 1
		out["trigger"] = t2
	}
	if components, ok := out["components"].([]interface{}); ok {
		newComponents := make([]interface{}, len(components))
		for i, c := range components {
			cm, ok := c.(map[string]interface{})
			if !ok {
				newComponents[i] = c
				continue
			}
			c2 := make(map[string]interface{}, len(cm)+3)
			for k, v := range cm {
				c2[k] = v
			}
			c2["id"] = fmt.Sprintf("component-server-id-%d", i)
			c2["schemaVersion"] = 1
			c2["conditions"] = []interface{}{}
			newComponents[i] = c2
		}
		out["components"] = newComponents
	}
	return out
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

func (m *automationRuleMock) setInjectExtraScopeARI(ari string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.injectExtraScopeARI = ari
}

func (m *automationRuleMock) setInjectServerNoiseOnGet(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.injectServerNoiseOnGet = v
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

// bodyRawJSONUpdated is a second `body` value (different assignee) used to
// exercise Update.
const bodyRawJSONUpdated = `<<-EOT
    {"trigger":{"type":"jira.manual.trigger.trigger","component":"TRIGGER"},"components":[{"value":{"assignee":"reporter"},"type":"jira.issue.assign","component":"ACTION"}]}
    EOT
`

// automationRuleConfigWithName is automationRuleConfig with an overridable
// rule name, for exercising Update.
func automationRuleConfigWithName(serverURL, name, bodyExpr string) string {
	return fmt.Sprintf(`provider "atlassian" {
  url                 = %[1]q
  automation_base_url = %[1]q
}

resource "atlassian_jira_automation_rule" "test" {
  name        = %[3]q
  project_ids = ["10549"]
  body        = %[2]s
}
`, serverURL, bodyExpr, name)
}

// automationRuleConfigWithProjectIDs is automationRuleConfig with an
// overridable project_ids set, for exercising project_ids updates and
// order-insensitivity.
func automationRuleConfigWithProjectIDs(serverURL, bodyExpr string, projectIDs []string) string {
	quoted := make([]string, len(projectIDs))
	for i, id := range projectIDs {
		quoted[i] = fmt.Sprintf("%q", id)
	}
	return fmt.Sprintf(`provider "atlassian" {
  url                 = %[1]q
  automation_base_url = %[1]q
}

resource "atlassian_jira_automation_rule" "test" {
  name        = "K-CARE ticket watcher"
  project_ids = [%[3]s]
  body        = %[2]s
}
`, serverURL, bodyExpr, strings.Join(quoted, ", "))
}

// automationRuleConfigWithState is automationRuleConfig with an overridable
// `state`, for exercising state updates.
func automationRuleConfigWithState(serverURL, bodyExpr, state string) string {
	return fmt.Sprintf(`provider "atlassian" {
  url                 = %[1]q
  automation_base_url = %[1]q
}

resource "atlassian_jira_automation_rule" "test" {
  name        = "K-CARE ticket watcher"
  project_ids = ["10549"]
  body        = %[2]s
  state       = %[3]q
}
`, serverURL, bodyExpr, state)
}

// TestAccAutomationRuleResource_UpdateNameAndBody covers T6 acceptance item
// (a): changing name and body must PUT /rule/{uuid} with the full document,
// carrying the new body's trigger/components and the rule's uuid.
func TestAccAutomationRuleResource_UpdateNameAndBody(t *testing.T) {
	mock := newAutomationRuleMock()
	serverURL := setupAutomationRuleMock(t, mock)

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: automationRuleConfig(serverURL, bodyRawJSON),
				Check:  resource.TestCheckResourceAttrSet("atlassian_jira_automation_rule.test", "uuid"),
			},
			{
				Config: automationRuleConfigWithName(serverURL, "K-CARE ticket watcher v2", bodyRawJSONUpdated),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_automation_rule.test", "name", "K-CARE ticket watcher v2"),
					func(_ *terraform.State) error {
						put := mock.lastPutRule()
						if put == nil {
							return fmt.Errorf("mock recorded no PUT /rule body")
						}
						if put["name"] != "K-CARE ticket watcher v2" {
							return fmt.Errorf("unexpected name in PUT body: %v", put["name"])
						}
						if v, ok := put["uuid"]; !ok || v == "" {
							return fmt.Errorf("PUT body must carry uuid, got %v", v)
						}
						if !jsonEquivalent(put["components"], []interface{}{
							map[string]interface{}{
								"component": "ACTION",
								"type":      "jira.issue.assign",
								"value":     map[string]interface{}{"assignee": "reporter"},
							},
						}) {
							return fmt.Errorf("unexpected components in PUT body: %v", put["components"])
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAccAutomationRuleResource_UpdateProjectIDsCallsRuleScopeAndPreservesExtraARIs
// covers T6 acceptance item (b): changing project_ids must PUT
// /rule/{uuid}/rule-scope with the new project ARIs, and must not drop a
// non-project scope ARI the server already recorded (exposed read-only via
// extra_scope_aris).
func TestAccAutomationRuleResource_UpdateProjectIDsCallsRuleScopeAndPreservesExtraARIs(t *testing.T) {
	mock := newAutomationRuleMock()
	mock.setInjectExtraScopeARI("ari:cloud:jira:abc:board/999")
	serverURL := setupAutomationRuleMock(t, mock)

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: automationRuleConfig(serverURL, bodyRawJSON),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("atlassian_jira_automation_rule.test", "uuid"),
					resource.TestCheckResourceAttr("atlassian_jira_automation_rule.test", "extra_scope_aris.#", "1"),
					resource.TestCheckResourceAttr("atlassian_jira_automation_rule.test", "extra_scope_aris.0", "ari:cloud:jira:abc:board/999"),
				),
			},
			{
				Config: automationRuleConfigWithProjectIDs(serverURL, bodyRawJSON, []string{"10550"}),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_automation_rule.test", "extra_scope_aris.0", "ari:cloud:jira:abc:board/999"),
					func(_ *terraform.State) error {
						call := mock.lastScopeCall()
						if call == nil {
							return fmt.Errorf("mock recorded no PUT .../rule-scope call")
						}
						want := []string{"ari:cloud:jira:abc:project/10550", "ari:cloud:jira:abc:board/999"}
						if len(call.aris) != len(want) {
							return fmt.Errorf("rule-scope ARIs: got %v, want %v", call.aris, want)
						}
						for i := range want {
							if call.aris[i] != want[i] {
								return fmt.Errorf("rule-scope ARIs: got %v, want %v", call.aris, want)
							}
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAccAutomationRuleResource_UpdateStateCallsStateEndpoint covers T6
// acceptance item (c): flipping ENABLED -> DISABLED must PUT
// /rule/{uuid}/state with {"state": "DISABLED"}.
func TestAccAutomationRuleResource_UpdateStateCallsStateEndpoint(t *testing.T) {
	mock := newAutomationRuleMock()
	serverURL := setupAutomationRuleMock(t, mock)

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: automationRuleConfig(serverURL, bodyRawJSON),
				Check:  resource.TestCheckResourceAttr("atlassian_jira_automation_rule.test", "state", "ENABLED"),
			},
			{
				Config: automationRuleConfigWithState(serverURL, bodyRawJSON, "DISABLED"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_automation_rule.test", "state", "DISABLED"),
					func(_ *terraform.State) error {
						call := mock.lastStateCall()
						if call == nil {
							return fmt.Errorf("mock recorded no PUT .../state call")
						}
						if call.state != "DISABLED" {
							return fmt.Errorf("state PUT: got %q, want DISABLED", call.state)
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAccAutomationRuleResource_DestroyDisablesBeforeDeleting covers T6
// acceptance item (d): destroy must PUT /rule/{uuid}/state {"state":
// "DISABLED"} before DELETE /rule/{uuid} — enforced for real by the mock's
// DELETE handler (400 unless the stored rule is already DISABLED).
func TestAccAutomationRuleResource_DestroyDisablesBeforeDeleting(t *testing.T) {
	mock := newAutomationRuleMock()
	serverURL := setupAutomationRuleMock(t, mock)

	var ruleUUID string

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		CheckDestroy: func(*terraform.State) error {
			if ruleUUID == "" {
				return fmt.Errorf("never captured a rule uuid to check destroy order against")
			}
			disableIdx := mock.callIndex("PUT state " + ruleUUID + " DISABLED")
			deleteIdx := mock.callIndex("DELETE " + ruleUUID)
			if disableIdx == -1 {
				return fmt.Errorf("expected a PUT .../state DISABLED call before delete, got calls %v", mock.calls)
			}
			if deleteIdx == -1 {
				return fmt.Errorf("expected a DELETE call, got calls %v", mock.calls)
			}
			if disableIdx > deleteIdx {
				return fmt.Errorf("expected disable-then-delete order, got calls %v", mock.calls)
			}
			if _, stillThere := mock.rules[ruleUUID]; stillThere {
				return fmt.Errorf("rule %s still present in mock after destroy", ruleUUID)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: automationRuleConfig(serverURL, bodyRawJSON),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("atlassian_jira_automation_rule.test", "uuid"),
					func(s *terraform.State) error {
						rs, ok := s.RootModule().Resources["atlassian_jira_automation_rule.test"]
						if !ok {
							return fmt.Errorf("resource not found in state")
						}
						ruleUUID = rs.Primary.Attributes["uuid"]
						if ruleUUID == "" {
							return fmt.Errorf("captured empty rule uuid")
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAccAutomationRuleResource_ReadIgnoresServerAddedNoiseInBody covers T6
// acceptance item (e): once the server starts echoing back trigger/
// components with an added "id"/"schemaVersion" and empty "conditions",
// (1) the next plan must stay empty, and (2) the `body` value stored in
// state must remain the user's original string rather than the server's
// reformatted one.
func TestAccAutomationRuleResource_ReadIgnoresServerAddedNoiseInBody(t *testing.T) {
	mock := newAutomationRuleMock()
	serverURL := setupAutomationRuleMock(t, mock)

	var capturedBody string

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: automationRuleConfig(serverURL, bodyRawJSON),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("atlassian_jira_automation_rule.test", "uuid"),
					func(s *terraform.State) error {
						rs, ok := s.RootModule().Resources["atlassian_jira_automation_rule.test"]
						if !ok {
							return fmt.Errorf("resource not found in state")
						}
						capturedBody = rs.Primary.Attributes["body"]
						if capturedBody == "" {
							return fmt.Errorf("captured body was empty")
						}
						return nil
					},
				),
			},
			{
				PreConfig: func() {
					mock.setInjectServerNoiseOnGet(true)
				},
				Config:             automationRuleConfig(serverURL, bodyRawJSON),
				PlanOnly:           true,
				ExpectNonEmptyPlan: false,
				Check: func(s *terraform.State) error {
					rs, ok := s.RootModule().Resources["atlassian_jira_automation_rule.test"]
					if !ok {
						return fmt.Errorf("resource not found in state")
					}
					if rs.Primary.Attributes["body"] != capturedBody {
						return fmt.Errorf("body changed after a Read with server-added noise: got %s, want unchanged %s",
							rs.Primary.Attributes["body"], capturedBody)
					}
					return nil
				},
			},
		},
	})
}

// TestAccAutomationRuleResource_ImportByUUID covers T6 acceptance item (f):
// `terraform import ... <uuid>` must populate every attribute via Read.
// `body` is checked with ImportStateCheck rather than ImportStateVerify
// (see that field's doc comment: preferred when an attribute round-trips
// with a syntactically different but semantically equal value) since the
// server re-serializes trigger/components with sorted keys.
func TestAccAutomationRuleResource_ImportByUUID(t *testing.T) {
	mock := newAutomationRuleMock()
	mock.setInjectExtraScopeARI("ari:cloud:jira:abc:board/999")
	serverURL := setupAutomationRuleMock(t, mock)

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: automationRuleConfig(serverURL, bodyRawJSON),
				Check:  resource.TestCheckResourceAttrSet("atlassian_jira_automation_rule.test", "uuid"),
			},
			{
				ResourceName: "atlassian_jira_automation_rule.test",
				ImportState:  true,
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("expected 1 imported instance state, got %d", len(states))
					}
					attrs := states[0].Attributes
					if attrs["name"] != "K-CARE ticket watcher" {
						return fmt.Errorf("imported name: got %q", attrs["name"])
					}
					if attrs["state"] != "ENABLED" {
						return fmt.Errorf("imported state: got %q", attrs["state"])
					}
					if attrs["uuid"] == "" {
						return fmt.Errorf("imported uuid is empty")
					}
					if attrs["id"] != attrs["uuid"] {
						return fmt.Errorf("imported id (%q) does not match uuid (%q)", attrs["id"], attrs["uuid"])
					}
					if attrs["project_ids.#"] != "1" {
						return fmt.Errorf("imported project_ids count: got %q (attrs: %#v)", attrs["project_ids.#"], attrs)
					}
					if attrs["extra_scope_aris.#"] != "1" || attrs["extra_scope_aris.0"] != "ari:cloud:jira:abc:board/999" {
						return fmt.Errorf("imported extra_scope_aris: got %#v", attrs)
					}
					if attrs["body"] == "" {
						return fmt.Errorf("imported body is empty")
					}
					return nil
				},
			},
		},
	})
}

// TestAccAutomationRuleResource_ProjectIDsOrderNoDiff covers T6 acceptance
// item (g): project_ids is a Set, so reordering it in config must not plan
// a diff.
func TestAccAutomationRuleResource_ProjectIDsOrderNoDiff(t *testing.T) {
	mock := newAutomationRuleMock()
	serverURL := setupAutomationRuleMock(t, mock)

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: automationRuleConfigWithProjectIDs(serverURL, bodyRawJSON, []string{"10549", "10550"}),
				Check:  resource.TestCheckResourceAttrSet("atlassian_jira_automation_rule.test", "uuid"),
			},
			{
				Config:             automationRuleConfigWithProjectIDs(serverURL, bodyRawJSON, []string{"10550", "10549"}),
				PlanOnly:           true,
				ExpectNonEmptyPlan: false,
			},
		},
	})
}

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

// The Automation Rule Management API addresses a rule's actor as
// {"type": "ACCOUNT_ID", "actor": "<accountId>"} (verified against a real site
// on 2026-09-21 — sending the id under "value" makes POST /rule answer 400
// "The request body could not be parsed"). Pin the wire shape so a refactor
// cannot silently reintroduce "value".
func TestAccAutomationRuleResource_ActorWireShape(t *testing.T) {
	mock := newAutomationRuleMock()
	serverURL := setupAutomationRuleMock(t, mock)

	config := fmt.Sprintf(`provider "atlassian" {
  url                 = %[1]q
  automation_base_url = %[1]q
}

resource "atlassian_jira_automation_rule" "test" {
  name             = "K-CARE ticket watcher"
  project_ids      = ["10549"]
  actor_account_id = "712020:bot"
  body             = %[2]s
}
`, serverURL, bodyRawJSON)

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_automation_rule.test", "actor_account_id", "712020:bot"),
					func(_ *terraform.State) error {
						raw := mock.lastPosted()
						if raw == nil {
							return fmt.Errorf("mock recorded no POST body")
						}
						actor, _ := raw["actor"].(map[string]interface{})
						if actor["type"] != "ACCOUNT_ID" || actor["actor"] != "712020:bot" {
							return fmt.Errorf(`actor must be sent as {"type":"ACCOUNT_ID","actor":<id>}, got %v`, raw["actor"])
						}
						if _, ok := actor["value"]; ok {
							return fmt.Errorf(`actor must not carry a "value" key, got %v`, raw["actor"])
						}
						return nil
					},
				),
			},
		},
	})
}

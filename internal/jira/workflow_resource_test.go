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

const workflowFixedEntityID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

// workflowMock models the versioned workflow API closely enough to exercise
// the resource end to end:
//   - GET  /rest/api/3/statuses/search          global statuses (paginated shape)
//   - POST /rest/api/3/workflows/create/validation, /update/validation
//   - POST /rest/api/3/workflows/create           assigns entityId, version 1, transition/rule ids
//   - POST /rest/api/3/workflows                  bulk get by ids or names
//   - POST /rest/api/3/workflows/update           version lock (409 on mismatch or when forced), optional taskId
//   - GET  /rest/api/3/task/{id}                  COMPLETE
//   - DELETE /rest/api/3/workflow/{entityId}
type workflowMock struct {
	mu        sync.Mutex
	statuses  []map[string]interface{}          // id, name, statusCategory
	workflows map[string]map[string]interface{} // entityId → document (with version, ids)
	nextRule  int
	// knobs
	conflictLeft  int  // next create/update answers 409 this many times
	asyncUpdate   bool // update answers with taskId and no workflow
	asyncCreate   bool // create answers with taskId and no workflow
	dropStatusIDs bool // bulk-get omits top-level statuses[].id (must fail Read loudly)
	rejectNamed   string
	updateBodies  []map[string]interface{}
	createBodies  []map[string]interface{}
}

func newWorkflowMock() *workflowMock {
	return &workflowMock{
		statuses: []map[string]interface{}{
			{"id": "11216", "name": "기안", "description": "신청자가 기안", "statusCategory": map[string]interface{}{"key": "TODO"}},
			{"id": "11217", "name": "검토 중", "description": "검토자가 검토", "statusCategory": map[string]interface{}{"key": "IN_PROGRESS"}},
			{"id": "11218", "name": "승인 대기", "description": "", "statusCategory": map[string]interface{}{"key": "IN_PROGRESS"}},
			{"id": "10373", "name": "완료", "description": "종결", "statusCategory": map[string]interface{}{"key": "DONE"}},
		},
		workflows: map[string]map[string]interface{}{},
		nextRule:  100,
	}
}

func (m *workflowMock) assignIDs(wf map[string]interface{}) {
	trs, _ := wf["transitions"].([]interface{})
	for i, raw := range trs {
		t := raw.(map[string]interface{})
		if id, _ := t["id"].(string); id == "" {
			t["id"] = fmt.Sprint(i + 1)
		}
		for _, k := range []string{"actions", "validators", "triggers"} {
			rules, _ := t[k].([]interface{})
			for _, r := range rules {
				rule := r.(map[string]interface{})
				if id, _ := rule["id"].(string); id == "" {
					rule["id"] = fmt.Sprintf("rule-%d", m.nextRule)
					m.nextRule++
				}
			}
		}
		if c, ok := t["conditions"].(map[string]interface{}); ok {
			rules, _ := c["conditions"].([]interface{})
			for _, r := range rules {
				rule := r.(map[string]interface{})
				if id, _ := rule["id"].(string); id == "" {
					rule["id"] = fmt.Sprintf("rule-%d", m.nextRule)
					m.nextRule++
				}
			}
		}
	}
}

func (m *workflowMock) statusDefsFor(wf map[string]interface{}) []map[string]interface{} {
	used := map[string]bool{}
	for _, s := range wf["statuses"].([]interface{}) {
		used[s.(map[string]interface{})["statusReference"].(string)] = true
	}
	out := []map[string]interface{}{}
	for _, s := range m.statuses {
		id := s["id"].(string)
		if used[id] {
			out = append(out, map[string]interface{}{
				"id": id, "statusReference": id, "name": s["name"],
				"statusCategory": s["statusCategory"].(map[string]interface{})["key"],
				"scope":          map[string]interface{}{"type": "GLOBAL"}, "description": s["description"],
			})
		}
	}
	return out
}

func (m *workflowMock) readResponse(wfs []map[string]interface{}) map[string]interface{} {
	statuses := []map[string]interface{}{}
	out := make([]map[string]interface{}, 0, len(wfs))
	for _, wf := range wfs {
		statuses = append(statuses, m.statusDefsFor(wf)...)
		out = append(out, serverOrdered(wf))
	}
	return map[string]interface{}{"statuses": statuses, "workflows": out}
}

// serverOrdered returns a shallow copy of wf whose transitions follow the
// order the real API answers with (observed on k-care-test): GLOBAL first,
// then INITIAL, then the rest — never the order they were sent in.
func serverOrdered(wf map[string]interface{}) map[string]interface{} {
	cp := make(map[string]interface{}, len(wf))
	for k, v := range wf {
		cp[k] = v
	}
	trs, _ := wf["transitions"].([]interface{})
	rank := func(t interface{}) int {
		switch t.(map[string]interface{})["type"] {
		case "GLOBAL":
			return 0
		case "INITIAL":
			return 1
		}
		return 2
	}
	ordered := make([]interface{}, 0, len(trs))
	for r := 0; r <= 2; r++ {
		for _, t := range trs {
			if rank(t) == r {
				ordered = append(ordered, t)
			}
		}
	}
	cp["transitions"] = ordered
	return cp
}

// checkWorkflowBody mirrors what Jira rejects: top-level statuses need
// id+statusReference+name+statusCategory; the first transition is INITIAL
// without conditions; GLOBAL transitions have no links; a restrict rule
// carries all seven parameter keys; update items carry statusMappings.
func checkWorkflowBody(body map[string]interface{}, isUpdate bool, statuses []map[string]interface{}) string {
	for _, raw := range body["statuses"].([]interface{}) {
		d := raw.(map[string]interface{})
		for _, k := range []string{"id", "statusReference", "name", "statusCategory"} {
			if v, _ := d[k].(string); v == "" {
				return "top-level status missing " + k
			}
		}
		// Real site (k-care-test, 2026-09-16): Jira upserts the top-level statuses from
		// this array — a missing description wiped the descriptions of all seven statuses.
		desc, has := d["description"].(string)
		if !has {
			return "top-level status missing description"
		}
		for _, s := range statuses {
			if s["id"] == d["id"] && s["description"] != desc {
				return fmt.Sprintf("top-level status %s description %q != current %q", d["id"], desc, s["description"])
			}
		}
	}
	for _, raw := range body["workflows"].([]interface{}) {
		wf := raw.(map[string]interface{})
		trs := wf["transitions"].([]interface{})
		if len(trs) == 0 || trs[0].(map[string]interface{})["type"] != "INITIAL" {
			return "first transition must be INITIAL"
		}
		seenIDs := map[string]bool{}
		for i, t := range trs {
			tr := t.(map[string]interface{})
			// Real API: "Missing required field 'payload.workflows.[0].transitions.[0].id'".
			if id, _ := tr["id"].(string); id == "" || seenIDs[id] {
				return fmt.Sprintf("Missing required field 'payload.workflows.[0].transitions.[%d].id'", i)
			} else {
				seenIDs[id] = true
			}
			if _, has := tr["conditions"]; has && i == 0 {
				return "INITIAL has conditions"
			}
			if tr["type"] == "GLOBAL" && len(tr["links"].([]interface{})) != 0 {
				return "GLOBAL transition has links"
			}
			if c, ok := tr["conditions"].(map[string]interface{}); ok {
				for _, r := range c["conditions"].([]interface{}) {
					rule := r.(map[string]interface{})
					if rule["ruleKey"] == "system:restrict-issue-transition" && len(rule["parameters"].(map[string]interface{})) != 7 {
						return "restrict rule must carry 7 parameter keys"
					}
				}
			}
		}
		if isUpdate {
			for _, k := range []string{"id", "version", "statusMappings", "defaultStatusMappings"} {
				if _, ok := wf[k]; !ok {
					return "update item missing " + k
				}
			}
			if _, ok := wf["name"]; ok {
				return "update item must not carry name"
			}
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (m *workflowMock) handler() http.HandlerFunc {
	wfPath := regexp.MustCompile(`^/rest/api/3/workflow/([^/]+)$`)
	taskPath := regexp.MustCompile(`^/rest/api/3/task/([^/]+)$`)
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/rest/api/3/statuses/search":
			writeJSON(w, 200, map[string]interface{}{"startAt": 0, "maxResults": 50, "total": len(m.statuses), "isLast": true, "values": m.statuses})

		case r.Method == "POST" && (r.URL.Path == "/rest/api/3/workflows/create/validation" || r.URL.Path == "/rest/api/3/workflows/update/validation"):
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			payload, _ := body["payload"].(map[string]interface{})
			errs := []map[string]interface{}{}
			for _, raw := range payload["workflows"].([]interface{}) {
				wf := raw.(map[string]interface{})
				if m.rejectNamed != "" && wf["name"] == m.rejectNamed {
					errs = append(errs, map[string]interface{}{"code": "NON_UNIQUE_WORKFLOW_NAME", "level": "ERROR", "type": "WORKFLOW", "message": "workflow name already used"})
				}
				for _, t := range wf["transitions"].([]interface{}) {
					tr := t.(map[string]interface{})
					if tr["type"] == "INITIAL" {
						if _, has := tr["conditions"]; has {
							errs = append(errs, map[string]interface{}{"code": "CONDITIONS_UNSUPPORTED_ON_INITIAL_TRANSITION", "level": "ERROR", "type": "TRANSITION", "message": "initial transitions cannot have conditions"})
						}
					}
				}
			}
			writeJSON(w, 200, map[string]interface{}{"errors": errs})

		case r.Method == "POST" && r.URL.Path == "/rest/api/3/workflows/create":
			if m.conflictLeft > 0 {
				m.conflictLeft--
				writeJSON(w, 409, map[string]interface{}{"errorMessages": []string{"Failed to acquire lock"}})
				return
			}
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if msg := checkWorkflowBody(body, false, m.statuses); msg != "" {
				writeJSON(w, 400, map[string]interface{}{"errorMessages": []string{msg}})
				return
			}
			m.createBodies = append(m.createBodies, body)
			wf := body["workflows"].([]interface{})[0].(map[string]interface{})
			wf["id"] = workflowFixedEntityID
			wf["version"] = map[string]interface{}{"id": "ver-1", "versionNumber": float64(1)}
			wf["scope"] = map[string]interface{}{"type": "GLOBAL"}
			wf["isEditable"] = true
			m.assignIDs(wf)
			m.workflows[workflowFixedEntityID] = wf
			if m.asyncCreate {
				writeJSON(w, 200, map[string]interface{}{"statuses": []interface{}{}, "workflows": []interface{}{}, "taskId": "task-7"})
				return
			}
			writeJSON(w, 200, m.readResponse([]map[string]interface{}{wf}))

		case r.Method == "POST" && r.URL.Path == "/rest/api/3/workflows":
			var body struct {
				WorkflowIDs   []string `json:"workflowIds"`
				WorkflowNames []string `json:"workflowNames"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			var found []map[string]interface{}
			for id, wf := range m.workflows {
				for _, want := range body.WorkflowIDs {
					if want == id {
						found = append(found, wf)
					}
				}
				for _, want := range body.WorkflowNames {
					if want == wf["name"] {
						found = append(found, wf)
					}
				}
			}
			if found == nil {
				// Real API (k-care-test, 2026-09-16): unknown id or name → 404, not an empty list.
				writeJSON(w, 404, map[string]interface{}{"errorMessages": []string{"Not found"}, "errors": map[string]interface{}{}})
				return
			}
			resp := m.readResponse(found)
			if m.dropStatusIDs {
				for _, d := range resp["statuses"].([]map[string]interface{}) {
					delete(d, "id")
				}
			}
			writeJSON(w, 200, resp)

		case r.Method == "POST" && r.URL.Path == "/rest/api/3/workflows/update":
			if m.conflictLeft > 0 {
				m.conflictLeft--
				writeJSON(w, 409, map[string]interface{}{"errorMessages": []string{"Failed to acquire lock"}})
				return
			}
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if msg := checkWorkflowBody(body, true, m.statuses); msg != "" {
				writeJSON(w, 400, map[string]interface{}{"errorMessages": []string{msg}})
				return
			}
			m.updateBodies = append(m.updateBodies, body)
			item := body["workflows"].([]interface{})[0].(map[string]interface{})
			id, _ := item["id"].(string)
			cur, ok := m.workflows[id]
			if !ok {
				w.WriteHeader(404)
				return
			}
			ver := item["version"].(map[string]interface{})
			curVer := cur["version"].(map[string]interface{})
			if ver["versionNumber"] != curVer["versionNumber"] {
				writeJSON(w, 409, map[string]interface{}{"errorMessages": []string{"version mismatch"}})
				return
			}
			for _, k := range []string{"description", "statuses", "transitions", "startPointLayout"} {
				if v, ok := item[k]; ok {
					cur[k] = v
				}
			}
			n := int(curVer["versionNumber"].(float64)) + 1
			cur["version"] = map[string]interface{}{"id": fmt.Sprintf("ver-%d", n), "versionNumber": float64(n)}
			m.assignIDs(cur)
			if m.asyncUpdate {
				writeJSON(w, 200, map[string]interface{}{"statuses": []interface{}{}, "workflows": []interface{}{}, "taskId": "task-7"})
				return
			}
			writeJSON(w, 200, m.readResponse([]map[string]interface{}{cur}))

		case r.Method == "GET" && taskPath.MatchString(r.URL.Path):
			writeJSON(w, 200, map[string]interface{}{"id": "task-7", "status": "COMPLETE"})

		case r.Method == "DELETE" && wfPath.MatchString(r.URL.Path):
			id := wfPath.FindStringSubmatch(r.URL.Path)[1]
			if _, ok := m.workflows[id]; !ok {
				w.WriteHeader(404)
				return
			}
			delete(m.workflows, id)
			w.WriteHeader(204)

		default:
			w.WriteHeader(404)
		}
	}
}

func (m *workflowMock) transition(name string) map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	wf, ok := m.workflows[workflowFixedEntityID]
	if !ok {
		return nil
	}
	for _, t := range wf["transitions"].([]interface{}) {
		tr := t.(map[string]interface{})
		if tr["name"] == name {
			return tr
		}
	}
	return nil
}

func setupWorkflowMock(t *testing.T, mock *workflowMock) {
	t.Helper()
	srv := httptest.NewServer(mock.handler())
	t.Cleanup(srv.Close)
	t.Setenv("ATLASSIAN_URL", srv.URL)
	t.Setenv("ATLASSIAN_USER", "test@test.com")
	t.Setenv("ATLASSIAN_TOKEN", "test-token")
}

const workflowConfigV1 = `resource "atlassian_jira_workflow" "test" {
  name        = "tf-test-kcare-infra"
  description = "v1"
  statuses = [
    { status_id = "11216" },
    { status_id = "11217" },
    { status_id = "11218" },
    { status_id = "10373" },
  ]
  transitions = [
    {
      name            = "검토 요청"
      from            = ["11216"]
      to              = "11217"
      allowed_groups  = ["g-1"]
      assign          = { type = "to-selected-user", account_id = "5b10ac8d82e05b22cc7d4ef5" }
      required_fields = ["description"]
    },
    {
      name                 = "검토 완료"
      from                 = ["11217"]
      to                   = "11218"
      allowed_groups       = ["g-2"]
      separation_of_duties = [{ from = "11216", to = "11217" }]
      assign               = { type = "to-reporter" }
    },
    {
      name          = "승인"
      from          = ["11218"]
      to            = "10373"
      allowed_roles = ["10002"]
    },
    {
      name = "재개"
      type = "GLOBAL"
      to   = "11216"
    },
  ]
}`

// v2: new group on 검토 요청, extra required field, 승인 dropped, new transition.
const workflowConfigV2 = `resource "atlassian_jira_workflow" "test" {
  name        = "tf-test-kcare-infra"
  description = "v2"
  statuses = [
    { status_id = "11216" },
    { status_id = "11217" },
    { status_id = "11218" },
    { status_id = "10373" },
  ]
  transitions = [
    {
      name            = "검토 요청"
      from            = ["11216"]
      to              = "11217"
      allowed_groups  = ["g-new"]
      assign          = { type = "to-selected-user", account_id = "5b10ac8d82e05b22cc7d4ef5" }
      required_fields = ["description", "customfield_10761"]
    },
    {
      name                 = "검토 완료"
      from                 = ["11217"]
      to                   = "11218"
      allowed_groups       = ["g-2"]
      separation_of_duties = [{ from = "11216", to = "11217" }]
      assign               = { type = "to-reporter" }
    },
    {
      name = "반려"
      from = ["11218"]
      to   = "11216"
    },
    {
      name = "재개"
      type = "GLOBAL"
      to   = "11216"
    },
  ]
}`

func TestAccWorkflowResource_CreateUpdateImport(t *testing.T) {
	mock := newWorkflowMock()
	setupWorkflowMock(t, mock)
	var restrictRuleID, transitionID string // captured after create, must survive the update

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		CheckDestroy: func(_ *terraform.State) error {
			mock.mu.Lock()
			defer mock.mu.Unlock()
			if _, ok := mock.workflows[workflowFixedEntityID]; ok {
				return fmt.Errorf("workflow still exists after destroy")
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: workflowConfigV1,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "id", workflowFixedEntityID),
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "version", "1"),
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "statuses.#", "4"),
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "transitions.#", "4"),
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "transitions.0.type", "DIRECTED"),
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "transitions.0.allowed_groups.0", "g-1"),
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "transitions.0.assign.account_id", "5b10ac8d82e05b22cc7d4ef5"),
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "transitions.1.separation_of_duties.0.from", "11216"),
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "transitions.3.type", "GLOBAL"),
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "transitions.3.from.#", "0"),
					func(_ *terraform.State) error {
						mock.mu.Lock()
						defer mock.mu.Unlock()
						body := mock.createBodies[0]
						if body["scope"].(map[string]interface{})["type"] != "GLOBAL" {
							return fmt.Errorf("create must be GLOBAL scope: %v", body["scope"])
						}
						defs := body["statuses"].([]interface{})
						if len(defs) != 4 || defs[0].(map[string]interface{})["name"] != "기안" || defs[0].(map[string]interface{})["statusReference"] != "11216" {
							return fmt.Errorf("top-level statuses must carry id=reference, name, category: %v", defs[0])
						}
						trs := body["workflows"].([]interface{})[0].(map[string]interface{})["transitions"].([]interface{})
						if trs[0].(map[string]interface{})["type"] != "INITIAL" {
							return fmt.Errorf("first transition must be INITIAL")
						}
						return nil
					},
					func(_ *terraform.State) error {
						tr := mock.transition("검토 요청")
						transitionID, _ = tr["id"].(string)
						cond := tr["conditions"].(map[string]interface{})["conditions"].([]interface{})[0].(map[string]interface{})
						restrictRuleID, _ = cond["id"].(string)
						if transitionID == "" || restrictRuleID == "" {
							return fmt.Errorf("mock did not assign ids: %v", tr)
						}
						return nil
					},
				),
			},
			{
				// No-op apply: plan must be empty (Read reproduces the config).
				Config:   workflowConfigV1,
				PlanOnly: true,
			},
			{
				Config: workflowConfigV2,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "version", "2"),
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "description", "v2"),
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "transitions.0.allowed_groups.0", "g-new"),
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "transitions.0.required_fields.#", "2"),
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "transitions.2.name", "반려"),
					func(_ *terraform.State) error {
						// The restrict rule and the transition keep their server ids across the update.
						tr := mock.transition("검토 요청")
						if tr == nil || tr["id"] != transitionID {
							return fmt.Errorf("transition id not preserved (want %s): %v", transitionID, tr)
						}
						cond := tr["conditions"].(map[string]interface{})["conditions"].([]interface{})[0].(map[string]interface{})
						if cond["id"] != restrictRuleID || cond["parameters"].(map[string]interface{})["groupIds"] != "g-new" {
							return fmt.Errorf("restrict rule id/params (want id %s): %v", restrictRuleID, cond)
						}
						if mock.transition("승인") != nil {
							return fmt.Errorf("removed transition still on server")
						}
						mock.mu.Lock()
						defer mock.mu.Unlock()
						upd := mock.updateBodies[0]["workflows"].([]interface{})[0].(map[string]interface{})
						if upd["version"].(map[string]interface{})["id"] != "ver-1" || upd["version"].(map[string]interface{})["versionNumber"] != float64(1) {
							return fmt.Errorf("update must send the current version: %v", upd["version"])
						}
						if _, ok := upd["statusMappings"]; !ok {
							return fmt.Errorf("update must send statusMappings (empty)")
						}
						return nil
					},
				),
			},
			{
				// Import has no prior state to order by, so transitions arrive in
				// server order; verify membership instead of positions.
				ResourceName:            "atlassian_jira_workflow.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"transitions"},
				ImportStateCheck: func(states []*terraform.InstanceState) error {
					if len(states) != 1 {
						return fmt.Errorf("expected 1 imported state, got %d", len(states))
					}
					a := states[0].Attributes
					if a["transitions.#"] != "4" {
						return fmt.Errorf("imported transitions.# = %q", a["transitions.#"])
					}
					names := map[string]bool{}
					for i := 0; i < 4; i++ {
						names[a[fmt.Sprintf("transitions.%d.name", i)]] = true
					}
					for _, want := range []string{"검토 요청", "검토 완료", "반려", "재개"} {
						if !names[want] {
							return fmt.Errorf("imported transitions missing %q: %v", want, names)
						}
					}
					return nil
				},
			},
		},
	})
}

func TestAccWorkflowResource_AsyncUpdateAndDrift(t *testing.T) {
	mock := newWorkflowMock()
	mock.asyncUpdate = true
	setupWorkflowMock(t, mock)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: workflowConfigV1},
			{
				Config: workflowConfigV2,
				Check:  resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "version", "2"), // read back after the task completed
			},
			{
				// Deleted out-of-band → Read removes → plan recreates.
				PreConfig: func() {
					mock.mu.Lock()
					delete(mock.workflows, workflowFixedEntityID)
					mock.mu.Unlock()
				},
				RefreshState:       true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

func TestAccWorkflowResource_ValidationErrorsSurface(t *testing.T) {
	mock := newWorkflowMock()
	mock.rejectNamed = "tf-test-kcare-infra"
	setupWorkflowMock(t, mock)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      workflowConfigV1,
				ExpectError: regexp.MustCompile(`NON_UNIQUE_WORKFLOW_NAME`),
			},
			{
				Config:      strings.Replace(workflowConfigV1, `to              = "11217"`, `to              = "99999"`, 1),
				ExpectError: regexp.MustCompile(`not in statuses`),
			},
		},
	})
}

// TFC apply (k-care-test, 2026-09-16): six workflows created in one apply made Jira
// answer 409 "Failed to acquire lock" repeatedly — one retry was not enough.
func TestAccWorkflowResource_RetriesRepeatedlyOn409(t *testing.T) {
	mock := newWorkflowMock()
	mock.conflictLeft = 3 // first three creates answer 409
	setupWorkflowMock(t, mock)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: workflowConfigV1,
				Check:  resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "version", "1"),
			},
			{
				PreConfig: func() {
					mock.mu.Lock()
					mock.conflictLeft = 3 // first three updates answer 409
					mock.mu.Unlock()
				},
				Config: workflowConfigV2,
				Check:  resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "version", "2"),
			},
		},
	})
}

func TestAccWorkflowResource_AsyncCreateReadsBackByName(t *testing.T) {
	mock := newWorkflowMock()
	mock.asyncCreate = true
	setupWorkflowMock(t, mock)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: workflowConfigV1,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "id", workflowFixedEntityID),
					resource.TestCheckResourceAttr("atlassian_jira_workflow.test", "version", "1"),
				),
			},
		},
	})
}

func TestAccWorkflowResource_ReadFailsLoudlyWithoutStatusIDs(t *testing.T) {
	mock := newWorkflowMock()
	setupWorkflowMock(t, mock)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testutil.ProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{Config: workflowConfigV1},
			{
				PreConfig: func() {
					mock.mu.Lock()
					mock.dropStatusIDs = true
					mock.mu.Unlock()
				},
				RefreshState: true,
				ExpectError:  regexp.MustCompile(`status definition without id`),
			},
		},
	})
}

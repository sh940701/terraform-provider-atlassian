package jira

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The versioned workflow API is a full-document API: whatever we do not
// model must still round-trip byte-for-byte in meaning, otherwise an update
// silently drops server-side configuration. These tests pin the wire types
// against two real payloads captured on 2026-09-15:
//   - workflow_read_project_scope.json  — POST /rest/api/3/workflows response (bulk get)
//   - workflow_create_global_scope.json — POST /rest/api/3/workflows/create body that validated with 0 errors

func loadJSON(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

func canonical(t *testing.T, raw []byte) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return v
}

func TestWorkflowReadResponse_RoundTripsRealPayload(t *testing.T) {
	raw := loadJSON(t, "workflow_read_project_scope.json")

	var resp workflowReadResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Workflows) != 1 {
		t.Fatalf("workflows: want 1, got %d", len(resp.Workflows))
	}
	wf := resp.Workflows[0]
	if wf.ID == "" || wf.Version.VersionNumber != 6 || wf.Scope.Type != "PROJECT" {
		t.Fatalf("header not decoded: id=%q version=%d scope=%q", wf.ID, wf.Version.VersionNumber, wf.Scope.Type)
	}
	if len(resp.Statuses) != 9 || resp.Statuses[0].ID == "" || resp.Statuses[0].StatusReference == "" {
		t.Fatalf("top-level statuses not decoded: %+v", resp.Statuses[:1])
	}

	// Every field the server sent must survive unmarshal → marshal.
	// (Comparing decoded trees ignores key order and number formatting.)
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !reflect.DeepEqual(canonical(t, raw), canonical(t, out)) {
		t.Fatalf("round trip lost data:\n want %s\n got  %s", raw, out)
	}
}

func TestWorkflowCreateRequest_RoundTripsValidatedPayload(t *testing.T) {
	raw := loadJSON(t, "workflow_create_global_scope.json")

	var req workflowCreateRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Scope.Type != "GLOBAL" || len(req.Statuses) != 4 || len(req.Workflows) != 1 {
		t.Fatalf("header: scope=%q statuses=%d workflows=%d", req.Scope.Type, len(req.Statuses), len(req.Workflows))
	}
	tr := req.Workflows[0].Transitions
	if tr[0].Type != "INITIAL" || tr[0].Conditions != nil {
		t.Fatalf("INITIAL transition must carry no conditions key: %+v", tr[0])
	}
	if got := tr[1].Conditions.Conditions[0].RuleKey; got != "system:restrict-issue-transition" {
		t.Fatalf("condition rule key: %q", got)
	}
	if got := tr[1].Actions[0].RuleKey; got != "system:change-assignee" {
		t.Fatalf("action rule key: %q", got)
	}

	out, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !reflect.DeepEqual(canonical(t, raw), canonical(t, out)) {
		t.Fatalf("round trip lost data:\n want %s\n got  %s", raw, out)
	}
}

func TestWorkflowTransition_OmitsEmptyOptionalCollections(t *testing.T) {
	// A GLOBAL transition assembled by us has no links and no rules; the
	// wire form must not invent `"conditions": null` (the API rejects
	// conditions on INITIAL and treats null oddly elsewhere).
	tr := workflowTransition{Name: "Reopen", Type: "GLOBAL", ToStatusReference: "ref-1"}
	out, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	_ = json.Unmarshal(out, &m)
	for _, k := range []string{"conditions", "id", "customIssueEventId", "transitionScreen", "description"} {
		if _, ok := m[k]; ok {
			t.Errorf("key %q must be omitted when empty, got %s", k, out)
		}
	}
	for _, k := range []string{"actions", "validators", "triggers", "links", "properties"} {
		if _, ok := m[k]; !ok {
			t.Errorf("key %q must be present (empty) so the server sees an explicit empty collection, got %s", k, out)
		}
	}
}

package jira

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The model layer is pure: Terraform-shaped spec ⇄ versioned-API document.
// It is exercised against the same real payloads as the wire types.

func readDoc(t *testing.T) (jiraWorkflow, map[string]string) {
	t.Helper()
	var resp workflowReadResponse
	if err := json.Unmarshal(loadJSON(t, "workflow_read_project_scope.json"), &resp); err != nil {
		t.Fatal(err)
	}
	refToID, err := statusRefIndex(resp.Statuses)
	if err != nil {
		t.Fatal(err)
	}
	return resp.Workflows[0], refToID
}

var sampleStatusDefs = map[string]workflowStatusDef{
	"11216": {ID: "11216", Name: "기안", StatusCategory: "TODO"},
	"11217": {ID: "11217", Name: "검토 중", StatusCategory: "IN_PROGRESS"},
	"11218": {ID: "11218", Name: "승인 대기", StatusCategory: "IN_PROGRESS"},
	"10373": {ID: "10373", Name: "완료", StatusCategory: "DONE"},
}

func sampleSpec() workflowSpec {
	return workflowSpec{
		Name:        "K-CARE 인프라 변경",
		Description: "절차서: drive:abc",
		StatusIDs:   []string{"11216", "11217", "11218", "10373"},
		Transitions: []workflowTransitionSpec{
			{Name: "검토 요청", Type: "DIRECTED", From: []string{"11216"}, To: "11217",
				AllowedGroups:  []string{"g-1"},
				Assign:         &workflowAssignSpec{Type: "to-selected-user", AccountID: "5b10ac8d82e05b22cc7d4ef5"},
				RequiredFields: []string{"description"}},
			{Name: "검토 완료", Type: "DIRECTED", From: []string{"11217"}, To: "11218",
				AllowedGroups:      []string{"g-2"},
				SeparationOfDuties: []workflowSoDSpec{{From: "11216", To: "11217"}},
				Assign:             &workflowAssignSpec{Type: "to-reporter"}},
			{Name: "승인", Type: "DIRECTED", From: []string{"11218"}, To: "10373", AllowedRoles: []string{"10002"}},
			{Name: "재개", Type: "GLOBAL", To: "11216"},
		},
	}
}

func TestBuildCreateRequest_ShapesDocumentLikeValidatedPayload(t *testing.T) {
	req, err := buildCreateRequest(sampleSpec(), sampleStatusDefs)
	if err != nil {
		t.Fatal(err)
	}
	if req.Scope.Type != "GLOBAL" {
		t.Fatalf("scope: %q", req.Scope.Type)
	}
	// Existing statuses: id and statusReference both the numeric id (validated 2026-09-15, probe B2).
	if len(req.Statuses) != 4 || req.Statuses[0].ID != "11216" || req.Statuses[0].StatusReference != "11216" || req.Statuses[0].Name != "기안" {
		t.Fatalf("top-level statuses: %+v", req.Statuses)
	}
	wf := req.Workflows[0]
	if len(wf.Transitions) != 5 {
		t.Fatalf("transitions: want INITIAL + 4, got %d", len(wf.Transitions))
	}
	init := wf.Transitions[0]
	if init.Type != "INITIAL" || init.Name != "Create" || init.ToStatusReference != "11216" || init.Conditions != nil || len(init.Links) != 0 {
		t.Fatalf("INITIAL: %+v", init)
	}
	tr := wf.Transitions[1]
	if tr.Type != "DIRECTED" || len(tr.Links) != 1 || tr.Links[0].FromStatusReference != "11216" || tr.Links[0].ToPort != 1 {
		t.Fatalf("DIRECTED links: %+v", tr.Links)
	}
	if got := tr.Conditions.Conditions[0]; got.RuleKey != "system:restrict-issue-transition" || got.Parameters["groupIds"] != "g-1" || len(got.Parameters) != 7 {
		t.Fatalf("restrict rule must carry all 7 keys: %+v", got)
	}
	if got := tr.Actions[0]; got.RuleKey != "system:change-assignee" || got.Parameters["type"] != "to-selected-user" || got.Parameters["accountId"] != "5b10ac8d82e05b22cc7d4ef5" {
		t.Fatalf("assign rule: %+v", got)
	}
	if got := tr.Validators[0]; got.RuleKey != "system:validate-field-value" || got.Parameters["fieldsRequired"] != "description" || got.Parameters["ruleType"] != "fieldRequired" {
		t.Fatalf("required field validator: %+v", got)
	}
	sod := wf.Transitions[2].Conditions.Conditions
	if len(sod) != 2 || sod[1].RuleKey != "system:separation-of-duties" || sod[1].Parameters["fromStatusId"] != "11216" || sod[1].Parameters["toStatusId"] != "11217" {
		t.Fatalf("SoD rule: %+v", sod)
	}
	if g := wf.Transitions[4]; g.Type != "GLOBAL" || len(g.Links) != 0 || g.Conditions != nil {
		t.Fatalf("GLOBAL transition must have no links and no conditions key: %+v", g)
	}
	// The whole thing must serialize (MarshalJSON hooks) without error.
	if _, err := json.Marshal(req); err != nil {
		t.Fatal(err)
	}
}

func TestBuildCreateRequest_RejectsUnknownStatusAndBadShape(t *testing.T) {
	s := sampleSpec()
	s.Transitions[0].To = "99999"
	if _, err := buildCreateRequest(s, sampleStatusDefs); err == nil {
		t.Fatal("expected error for transition to a status not in statuses")
	}
	s = sampleSpec()
	s.Transitions[0].From = nil
	if _, err := buildCreateRequest(s, sampleStatusDefs); err == nil {
		t.Fatal("expected error for DIRECTED transition without from")
	}
	s = sampleSpec()
	s.Transitions[1].Name = s.Transitions[0].Name
	if _, err := buildCreateRequest(s, sampleStatusDefs); err == nil {
		t.Fatal("expected error for duplicate transition names")
	}
	s = sampleSpec()
	s.Transitions[0].Assign = &workflowAssignSpec{Type: "to-selected-user"}
	if _, err := buildCreateRequest(s, sampleStatusDefs); err == nil {
		t.Fatal("expected error for to-selected-user without account_id")
	}
}

func TestSpecFromDocument_ReadsRealWorkflow(t *testing.T) {
	doc, refToID := readDoc(t)
	spec, err := specFromDocument(doc, refToID)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != doc.Name || len(spec.StatusIDs) != len(doc.Statuses) {
		t.Fatalf("header: %+v", spec)
	}
	// INITIAL is not exposed; 9 server transitions → 8 in the spec.
	if len(spec.Transitions) != 8 {
		t.Fatalf("transitions: want 8, got %d", len(spec.Transitions))
	}
	byName := map[string]workflowTransitionSpec{}
	for _, tr := range spec.Transitions {
		byName[tr.Name] = tr
	}
	req := byName["검토 요청"]
	if req.Type != "DIRECTED" || !reflect.DeepEqual(req.From, []string{"11250"}) || req.To != "11248" {
		t.Fatalf("검토 요청 topology: %+v", req)
	}
	if !reflect.DeepEqual(req.AllowedGroups, []string{"00000000-0000-0000-0000-000000000001"}) || len(req.AllowedRoles) != 0 {
		t.Fatalf("검토 요청 restrict: %+v", req)
	}
	if req.Assign == nil || req.Assign.Type != "to-selected-user" || req.Assign.AccountID != "5b10ac8d82e05b22cc7d4ef5" {
		t.Fatalf("검토 요청 assign: %+v", req.Assign)
	}
	if !reflect.DeepEqual(req.RequiredFields, []string{"description"}) {
		t.Fatalf("검토 요청 required: %+v", req.RequiredFields)
	}
	appr := byName["승인"]
	if len(appr.SeparationOfDuties) != 2 {
		t.Fatalf("승인 must expose both separation-of-duties rules: %+v", appr.SeparationOfDuties)
	}
}

func TestBuildUpdateItem_UnchangedSpecRoundTripsServerDocument(t *testing.T) {
	// Read → spec → update document must reproduce the server document
	// exactly (rule ids, transition ids, layouts, properties) — otherwise a
	// no-op apply would rewrite the workflow.
	doc, refToID := readDoc(t)
	spec, err := specFromDocument(doc, refToID)
	if err != nil {
		t.Fatal(err)
	}
	defs := map[string]workflowStatusDef{}
	for ref, id := range refToID {
		defs[id] = workflowStatusDef{ID: id, StatusReference: ref, Name: "n", StatusCategory: "TODO"}
	}
	item, err := buildUpdateItem(spec, doc, defs)
	if err != nil {
		t.Fatal(err)
	}
	// Compare on the wire (nil vs empty slices are the same JSON).
	want, _ := json.Marshal(doc.Transitions)
	got, _ := json.Marshal(item.Transitions)
	if string(want) != string(got) {
		t.Fatalf("transitions changed on no-op update:\nwant %s\ngot  %s", want, got)
	}
	wantS, _ := json.Marshal(doc.Statuses)
	gotS, _ := json.Marshal(item.Statuses)
	if string(wantS) != string(gotS) || item.Version == nil || item.Version.VersionNumber != 6 || item.ID != doc.ID {
		t.Fatalf("statuses/version/id not carried: %+v", item)
	}
	if item.StartPointLayout == nil {
		t.Fatal("startPointLayout dropped")
	}
}

func TestBuildUpdateItem_ChangedRuleKeepsIdAndUnknownRules(t *testing.T) {
	doc, refToID := readDoc(t)
	spec, _ := specFromDocument(doc, refToID)
	defs := map[string]workflowStatusDef{}
	for ref, id := range refToID {
		defs[id] = workflowStatusDef{ID: id, StatusReference: ref, Name: "n", StatusCategory: "TODO"}
	}

	// Plant an unmanaged rule on «검토 요청» that the spec knows nothing about.
	for i := range doc.Transitions {
		if doc.Transitions[i].Name == "검토 요청" {
			doc.Transitions[i].Validators = append(doc.Transitions[i].Validators,
				workflowRule{ID: "keep-me", RuleKey: "system:check-permission-validator", Parameters: map[string]string{"permissionKey": "ADMINISTER_PROJECTS"}})
			doc.Transitions[i].Triggers = []workflowRule{{ID: "trg", RuleKey: "system:development-triggers", Parameters: map[string]string{"enabledTriggers": "commit-created-trigger"}}}
		}
	}

	// Change the allowed group and add a required field on «검토 요청».
	for i := range spec.Transitions {
		if spec.Transitions[i].Name == "검토 요청" {
			spec.Transitions[i].AllowedGroups = []string{"g-new"}
			spec.Transitions[i].RequiredFields = []string{"description", "customfield_10761"}
		}
	}
	item, err := buildUpdateItem(spec, doc, defs)
	if err != nil {
		t.Fatal(err)
	}
	var tr *workflowTransition
	for i := range item.Transitions {
		if item.Transitions[i].Name == "검토 요청" {
			tr = &item.Transitions[i]
		}
	}
	if tr == nil {
		t.Fatal("transition lost")
	}
	if tr.ID != "6" {
		t.Fatalf("transition id must be preserved, got %q", tr.ID)
	}
	cond := tr.Conditions.Conditions[0]
	if cond.ID != "35739a78-8fe7-4be0-83d9-26358f71d2ff" || cond.Parameters["groupIds"] != "g-new" {
		t.Fatalf("restrict rule must keep its id and take the new group: %+v", cond)
	}
	var keep, trig, existingReq, newReq bool
	for _, v := range tr.Validators {
		switch {
		case v.RuleKey == "system:check-permission-validator" && v.ID == "keep-me":
			keep = true
		case v.RuleKey == "system:validate-field-value" && v.Parameters["fieldsRequired"] == "description" && v.ID == "99e5297e-9691-44a2-9350-80dba9589c1e":
			existingReq = true
		case v.RuleKey == "system:validate-field-value" && v.Parameters["fieldsRequired"] == "customfield_10761" && v.ID == "":
			newReq = true
		}
	}
	trig = len(tr.Triggers) == 1 && tr.Triggers[0].ID == "trg"
	if !keep || !trig || !existingReq || !newReq {
		t.Fatalf("rule merge wrong: keep=%v trig=%v existingReq=%v newReq=%v validators=%+v", keep, trig, existingReq, newReq, tr.Validators)
	}
	if tr.Actions[0].ID != "207757776" {
		t.Fatalf("assign rule id must be preserved: %+v", tr.Actions)
	}
}

func TestStatusRefIndex_RequiresIDs(t *testing.T) {
	if _, err := statusRefIndex([]workflowStatusDef{{StatusReference: "abc", Name: "x", StatusCategory: "TODO"}}); err == nil {
		t.Fatal("expected error when a status definition has no id")
	}
	if _, err := statusRefIndex(nil); err == nil {
		t.Fatal("expected error when the status index is empty")
	}
}

func TestValidateSpec_RejectsGlobalFromAndDuplicates(t *testing.T) {
	s := sampleSpec()
	s.Transitions[3].From = []string{"11216"} // GLOBAL with from
	if err := validateSpec(s); err == nil {
		t.Fatal("GLOBAL transition with `from` must be rejected")
	}
	s = sampleSpec()
	s.Transitions[1].SeparationOfDuties = append(s.Transitions[1].SeparationOfDuties, workflowSoDSpec{From: "11216", To: "11217"})
	if err := validateSpec(s); err == nil {
		t.Fatal("duplicate separation_of_duties pair must be rejected")
	}
	s = sampleSpec()
	s.Transitions[0].RequiredFields = []string{"description", "description"}
	if err := validateSpec(s); err == nil {
		t.Fatal("duplicate required field must be rejected")
	}
	s = sampleSpec()
	s.Transitions[0].Name = "Create"
	if err := validateSpec(s); err == nil {
		t.Fatal("reserved transition name Create must be rejected")
	}
}

func TestUpdateItem_WireOmitsReadOnlyDocumentFields(t *testing.T) {
	doc, refToID := readDoc(t)
	spec, _ := specFromDocument(doc, refToID)
	defs := map[string]workflowStatusDef{}
	for ref, id := range refToID {
		defs[id] = workflowStatusDef{ID: id, StatusReference: ref}
	}
	item, err := buildUpdateItem(spec, doc, defs)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(item)
	var m map[string]interface{}
	_ = json.Unmarshal(raw, &m)
	for _, k := range []string{"name", "scope", "isEditable", "created", "updated", "taskId"} {
		if _, ok := m[k]; ok {
			t.Errorf("update item must not carry %q", k)
		}
	}
	for _, k := range []string{"id", "version", "description", "statuses", "transitions", "statusMappings", "defaultStatusMappings"} {
		if _, ok := m[k]; !ok {
			t.Errorf("update item must carry %q", k)
		}
	}
}

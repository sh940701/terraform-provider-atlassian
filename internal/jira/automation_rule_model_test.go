package jira

import (
	"encoding/json"
	"testing"
)

func TestAriFromProjectIDAndBack(t *testing.T) {
	ari := ariFromProjectID("abc", "10549")
	want := "ari:cloud:jira:abc:project/10549"
	if ari != want {
		t.Fatalf("ariFromProjectID: got %q, want %q", ari, want)
	}

	id, ok := projectIDFromARI(ari)
	if !ok || id != "10549" {
		t.Fatalf("projectIDFromARI(%q): got (%q, %v), want (\"10549\", true)", ari, id, ok)
	}
}

func TestProjectIDFromARIRejectsOtherScopes(t *testing.T) {
	if _, ok := projectIDFromARI("ari:cloud:jira:abc:issue/1"); ok {
		t.Fatal("expected a non-project ARI to be rejected")
	}
}

func TestProjectIDFromARIRejectsTrailingExtraSegment(t *testing.T) {
	if id, ok := projectIDFromARI("ari:cloud:jira:abc:project/10549/extra"); ok {
		t.Fatalf("expected an ARI with a trailing extra segment to be rejected, got (%q, true)", id)
	}
}

func TestProjectIDsFromARIsSkipsNonProjectScopes(t *testing.T) {
	got := projectIDsFromARIs([]string{
		"ari:cloud:jira:abc:project/10549",
		"ari:cloud:jira:abc:issue/1",
		"ari:cloud:jira:abc:project/99",
	})
	want := []string{"10549", "99"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("projectIDsFromARIs: got %v, want %v", got, want)
	}
}

func TestDocFromBodyRoundTripsThroughBodyFromDoc(t *testing.T) {
	body := `{"trigger":{"type":"jira.manual.trigger.trigger"},"components":[{"type":"jira.issue.assign"}]}`
	trigger, components, err := docFromBody(body)
	if err != nil {
		t.Fatalf("docFromBody: unexpected error: %s", err)
	}

	doc := ruleDoc{Trigger: trigger, Components: components}
	got, err := bodyFromDoc(doc)
	if err != nil {
		t.Fatalf("bodyFromDoc: unexpected error: %s", err)
	}

	// Re-parse both sides rather than comparing bytes: bodyFromDoc is free to
	// reorder/re-marshal (Trigger/Components are only two fixed keys, but a
	// byte comparison would still be fragile to marshal quirks).
	rt, _, err := docFromBody(got)
	if err != nil {
		t.Fatalf("docFromBody(bodyFromDoc(...)): unexpected error: %s", err)
	}
	if string(rt) != string(trigger) {
		t.Errorf("round trip changed trigger: got %s, want %s", rt, trigger)
	}
}

func TestDocFromBodyRejectsMissingTriggerOrComponents(t *testing.T) {
	if _, _, err := docFromBody(`{"components":[]}`); err == nil {
		t.Error("expected an error for a body with no trigger")
	}
	if _, _, err := docFromBody(`{"trigger":{}}`); err == nil {
		t.Error("expected an error for a body with no components")
	}
}

func TestActorFromAccountIDAndBack(t *testing.T) {
	if a := actorFromAccountID(""); a != nil {
		t.Fatalf("actorFromAccountID(\"\"): got %+v, want nil", a)
	}

	a := actorFromAccountID("5b109f2e9729b51b54dc274d")
	if a == nil || a.Type != ruleActorTypeAccountID || a.Value != "5b109f2e9729b51b54dc274d" {
		t.Fatalf("actorFromAccountID: got %+v", a)
	}
	if got := a.accountID(); got != "5b109f2e9729b51b54dc274d" {
		t.Errorf("accountID(): got %q", got)
	}

	var nilActor *ruleActor
	if got := nilActor.accountID(); got != "" {
		t.Errorf("nil.accountID(): got %q, want \"\"", got)
	}
}

func TestDocFromRuleBuildsScopeARIsAndActor(t *testing.T) {
	body := `{"trigger":{"type":"t"},"components":[{"type":"c"}]}`
	doc, err := docFromRule("abc", "K-CARE watcher", "desc", "ENABLED", []string{"10549", "99"}, nil, body, "acct-1", true, "FIRSTERROR")
	if err != nil {
		t.Fatalf("docFromRule: unexpected error: %s", err)
	}
	want := []string{"ari:cloud:jira:abc:project/10549", "ari:cloud:jira:abc:project/99"}
	if len(doc.RuleScopeARIs) != 2 || doc.RuleScopeARIs[0] != want[0] || doc.RuleScopeARIs[1] != want[1] {
		t.Errorf("RuleScopeARIs: got %v, want %v", doc.RuleScopeARIs, want)
	}
	if doc.Actor == nil || doc.Actor.Value != "acct-1" {
		t.Errorf("Actor: got %+v", doc.Actor)
	}
	if doc.Name != "K-CARE watcher" || doc.Description != "desc" || doc.State != "ENABLED" {
		t.Errorf("doc: got %+v", doc)
	}
	if !doc.CanOtherRuleTrigger {
		t.Error("CanOtherRuleTrigger: got false, want true")
	}
	if doc.NotifyOnError != "FIRSTERROR" {
		t.Errorf("NotifyOnError: got %q, want %q", doc.NotifyOnError, "FIRSTERROR")
	}
}

func TestDocFromRuleKeepsExtraScopeARIsUnchanged(t *testing.T) {
	body := `{"trigger":{"type":"t"},"components":[{"type":"c"}]}`
	extra := []string{"ari:cloud:jira:abc:board/1"}
	doc, err := docFromRule("abc", "n", "", "ENABLED", []string{"10549"}, extra, body, "", false, "")
	if err != nil {
		t.Fatalf("docFromRule: unexpected error: %s", err)
	}
	want := []string{"ari:cloud:jira:abc:project/10549", "ari:cloud:jira:abc:board/1"}
	if len(doc.RuleScopeARIs) != 2 || doc.RuleScopeARIs[0] != want[0] || doc.RuleScopeARIs[1] != want[1] {
		t.Errorf("RuleScopeARIs: got %v, want %v", doc.RuleScopeARIs, want)
	}
}

func TestDocFromRulePropagatesBodyError(t *testing.T) {
	if _, err := docFromRule("abc", "n", "", "ENABLED", nil, nil, `{"trigger":{}}`, "", false, ""); err == nil {
		t.Error("expected docFromRule to propagate a body parse error")
	}
}

func TestExtraScopeARIsFromARIsSkipsProjectScopes(t *testing.T) {
	got := extraScopeARIsFromARIs([]string{
		"ari:cloud:jira:abc:project/10549",
		"ari:cloud:jira:abc:board/1",
		"ari:cloud:jira:abc:project/99",
		"ari:cloud:jira:abc:filter/2",
	})
	want := []string{"ari:cloud:jira:abc:board/1", "ari:cloud:jira:abc:filter/2"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("extraScopeARIsFromARIs: got %v, want %v", got, want)
	}
}

func TestScopeARIsFromProjectIDsAppendsExtrasAfterProjects(t *testing.T) {
	got := scopeARIsFromProjectIDs("abc", []string{"10549"}, []string{"ari:cloud:jira:abc:board/1"})
	want := []string{"ari:cloud:jira:abc:project/10549", "ari:cloud:jira:abc:board/1"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("scopeARIsFromProjectIDs: got %v, want %v", got, want)
	}
}

func TestNormalizeBodyRemovesServerAddedKeysAtAnyDepth(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "top-level-id",
		"schemaVersion": 1,
		"trigger": {"type": "t", "id": "nested-id"},
		"components": [
			{"type": "c", "schemaVersion": 2, "conditions": [], "children": []}
		],
		"conditions": [],
		"children": []
	}`)

	got, err := normalizeBody(raw)
	if err != nil {
		t.Fatalf("normalizeBody: unexpected error: %s", err)
	}

	var v map[string]interface{}
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatalf("re-unmarshaling normalized body: %s", err)
	}
	if _, ok := v["id"]; ok {
		t.Error("expected top-level id to be removed")
	}
	if _, ok := v["schemaVersion"]; ok {
		t.Error("expected top-level schemaVersion to be removed")
	}
	if _, ok := v["conditions"]; ok {
		t.Error("expected empty top-level conditions to be removed")
	}
	if _, ok := v["children"]; ok {
		t.Error("expected empty top-level children to be removed")
	}

	trigger, ok := v["trigger"].(map[string]interface{})
	if !ok {
		t.Fatalf("trigger: got %T, want map", v["trigger"])
	}
	if _, ok := trigger["id"]; ok {
		t.Error("expected nested trigger.id to be removed")
	}

	components, ok := v["components"].([]interface{})
	if !ok || len(components) != 1 {
		t.Fatalf("components: got %v", v["components"])
	}
	component, ok := components[0].(map[string]interface{})
	if !ok {
		t.Fatalf("components[0]: got %T, want map", components[0])
	}
	if _, ok := component["schemaVersion"]; ok {
		t.Error("expected nested component.schemaVersion to be removed")
	}
	if _, ok := component["conditions"]; ok {
		t.Error("expected nested empty component.conditions to be removed")
	}
	if _, ok := component["children"]; ok {
		t.Error("expected nested empty component.children to be removed")
	}
}

func TestNormalizeBodyKeepsNonEmptyConditionsAndChildren(t *testing.T) {
	raw := json.RawMessage(`{"conditions": [{"type": "c"}], "children": [{"type": "x"}]}`)
	got, err := normalizeBody(raw)
	if err != nil {
		t.Fatalf("normalizeBody: unexpected error: %s", err)
	}
	var v map[string]interface{}
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatalf("re-unmarshaling: %s", err)
	}
	if _, ok := v["conditions"]; !ok {
		t.Error("expected non-empty conditions to be kept")
	}
	if _, ok := v["children"]; !ok {
		t.Error("expected non-empty children to be kept")
	}
}

func TestNormalizeBodySortsObjectKeys(t *testing.T) {
	a, err := normalizeBody(json.RawMessage(`{"b": 1, "a": 2, "c": {"z": 1, "y": 2}}`))
	if err != nil {
		t.Fatalf("normalizeBody(a): unexpected error: %s", err)
	}
	b, err := normalizeBody(json.RawMessage(`{"c": {"y": 2, "z": 1}, "a": 2, "b": 1}`))
	if err != nil {
		t.Fatalf("normalizeBody(b): unexpected error: %s", err)
	}
	if string(a) != string(b) {
		t.Errorf("expected key-order-independent inputs to normalize identically: %s vs %s", a, b)
	}
	want := `{"a":2,"b":1,"c":{"y":2,"z":1}}`
	if string(a) != want {
		t.Errorf("normalizeBody: got %s, want %s", a, want)
	}
}

func TestNormalizeBodyIsIdempotent(t *testing.T) {
	raw := json.RawMessage(`{"id": "x", "trigger": {"type": "t", "id": "y"}, "conditions": []}`)
	once, err := normalizeBody(raw)
	if err != nil {
		t.Fatalf("normalizeBody (1st pass): unexpected error: %s", err)
	}
	twice, err := normalizeBody(once)
	if err != nil {
		t.Fatalf("normalizeBody (2nd pass): unexpected error: %s", err)
	}
	if string(once) != string(twice) {
		t.Errorf("normalizeBody is not idempotent: %s vs %s", once, twice)
	}
}

func TestBodyForStateKeepsOldStringWhenSemanticallyEqual(t *testing.T) {
	old := `{"trigger":{"type":"t","component":"TRIGGER"},"components":[{"type":"c"}]}`
	newBody := `{"trigger":{"id":"srv-1","schemaVersion":1,"component":"TRIGGER","type":"t"},"components":[{"type":"c","conditions":[]}]}`

	got, err := bodyForState(old, newBody)
	if err != nil {
		t.Fatalf("bodyForState: unexpected error: %s", err)
	}
	if got != old {
		t.Errorf("bodyForState: got %s, want the old string preserved (%s)", got, old)
	}
}

func TestBodyForStateUsesNewStringWhenDifferent(t *testing.T) {
	old := `{"trigger":{"type":"t"},"components":[{"type":"c"}]}`
	newBody := `{"trigger":{"type":"t2"},"components":[{"type":"c"}]}`

	got, err := bodyForState(old, newBody)
	if err != nil {
		t.Fatalf("bodyForState: unexpected error: %s", err)
	}
	if got != newBody {
		t.Errorf("bodyForState: got %s, want the new string (%s)", got, newBody)
	}
}

func TestBodyForStateUsesNewStringWhenNoPriorState(t *testing.T) {
	newBody := `{"trigger":{"type":"t"},"components":[{"type":"c"}]}`
	got, err := bodyForState("", newBody)
	if err != nil {
		t.Fatalf("bodyForState: unexpected error: %s", err)
	}
	if got != newBody {
		t.Errorf("bodyForState: got %s, want the new string (%s)", got, newBody)
	}
}

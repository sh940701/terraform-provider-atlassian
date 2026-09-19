package jira

import "testing"

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
	doc, err := docFromRule("abc", "K-CARE watcher", "desc", "ENABLED", []string{"10549", "99"}, body, "acct-1")
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
}

func TestDocFromRulePropagatesBodyError(t *testing.T) {
	if _, err := docFromRule("abc", "n", "", "ENABLED", nil, `{"trigger":{}}`, ""); err == nil {
		t.Error("expected docFromRule to propagate a body parse error")
	}
}

package jira

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// Model layer for atlassian_jira_automation_rule: converts between the
// Terraform shape and the Automation Rule Management API document
// (ruleDoc, https://api.atlassian.com/automation/public/jira). Pure
// functions; the resource layer only moves values in and out of framework
// types.
//
// `body` (a jsontypes.Normalized string) carries only the rule's trigger
// and components — the parts of the automation component graph this
// resource does not model. Everything else the API needs (scope, actor,
// name/description/state) has its own attribute and is assembled into the
// full ruleDoc by docFromRule.

// ruleDoc is the Automation Rule Management API document: the payload of
// POST /rule (wrapped as {"rule": ruleDoc}) and the shape GET
// /rule/{ruleUuid} returns at the top level (verified against a real site
// for GET; the POST response shape is unconfirmed — see
// automation_rule_resource.go's createRuleResponse).
type ruleDoc struct {
	Name                string          `json:"name"`
	Description         string          `json:"description,omitempty"`
	State               string          `json:"state"`
	Trigger             json.RawMessage `json:"trigger"`
	Components          json.RawMessage `json:"components"`
	RuleScopeARIs       []string        `json:"ruleScopeARIs"`
	Actor               *ruleActor      `json:"actor,omitempty"`
	WriteAccessType     string          `json:"writeAccessType,omitempty"`
	CanOtherRuleTrigger bool            `json:"canOtherRuleTrigger"`
	NotifyOnError       string          `json:"notifyOnError,omitempty"`
	UUID                string          `json:"uuid,omitempty"`
}

// ruleActor names who a rule's actions run as. The exact JSON shape is
// unverified against the real API (T5) — kept behind actorFromAccountID and
// (*ruleActor).accountID so a later task can adjust it in one place.
type ruleActor struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

const ruleActorTypeAccountID = "ACCOUNT_ID"

// actorFromAccountID builds the actor document for an account id, or nil
// when accountID is empty — the rule then runs as whatever Jira defaults to
// (typically its author).
func actorFromAccountID(accountID string) *ruleActor {
	if accountID == "" {
		return nil
	}
	return &ruleActor{Type: ruleActorTypeAccountID, Value: accountID}
}

// accountID returns the account id a as names, or "" for a nil actor or one
// not addressed by account id. Safe to call on a nil receiver.
func (a *ruleActor) accountID() string {
	if a == nil || a.Type != ruleActorTypeAccountID {
		return ""
	}
	return a.Value
}

// ruleBody is the JSON shape of the `body` attribute: a rule's trigger and
// components, without the scope/actor/metadata fields Terraform manages
// through their own attributes.
type ruleBody struct {
	Trigger    json.RawMessage `json:"trigger"`
	Components json.RawMessage `json:"components"`
}

// bodyFromDoc renders doc's trigger/components as the `body` JSON string.
func bodyFromDoc(doc ruleDoc) (string, error) {
	b, err := json.Marshal(ruleBody{Trigger: doc.Trigger, Components: doc.Components})
	if err != nil {
		return "", fmt.Errorf("encoding rule body: %w", err)
	}
	return string(b), nil
}

// docFromBody parses the `body` JSON string into trigger/components.
func docFromBody(body string) (trigger, components json.RawMessage, err error) {
	var parsed ruleBody
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return nil, nil, fmt.Errorf("parsing rule body: %w", err)
	}
	if len(parsed.Trigger) == 0 {
		return nil, nil, fmt.Errorf(`rule body must have a "trigger" object`)
	}
	if len(parsed.Components) == 0 {
		return nil, nil, fmt.Errorf(`rule body must have a "components" array`)
	}
	return parsed.Trigger, parsed.Components, nil
}

// projectScopeARIPattern matches the ARI shape for a rule scoped to one
// project: ari:cloud:jira:{cloudId}:project/{projectId}. The project id
// segment excludes "/" so a trailing extra segment
// (ari:cloud:jira:abc:project/10549/extra) is rejected rather than
// swallowed into the id.
var projectScopeARIPattern = regexp.MustCompile(`^ari:cloud:jira:[^:]+:project/([^/]+)$`)

// ariFromProjectID builds the project-scope ARI for projectID under cloudID.
func ariFromProjectID(cloudID, projectID string) string {
	return fmt.Sprintf("ari:cloud:jira:%s:project/%s", cloudID, projectID)
}

// projectIDFromARI extracts the project id from a project-scope ARI. ok is
// false for an ARI of a different shape (a scope this resource does not
// manage), which the caller skips rather than errors on.
func projectIDFromARI(ari string) (id string, ok bool) {
	m := projectScopeARIPattern.FindStringSubmatch(ari)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// projectIDsFromARIs extracts project ids from ruleScopeARIs, in order,
// skipping any ARI that is not a project scope.
func projectIDsFromARIs(aris []string) []string {
	ids := make([]string, 0, len(aris))
	for _, ari := range aris {
		if id, ok := projectIDFromARI(ari); ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// docFromRule assembles the create request document for a rule named name,
// scoped to cloudID's projectIDs, with the given body (trigger+components)
// and optional actor account id.
func docFromRule(cloudID, name, description, state string, projectIDs []string, body string, actorAccountID string) (ruleDoc, error) {
	trigger, components, err := docFromBody(body)
	if err != nil {
		return ruleDoc{}, err
	}
	aris := make([]string, 0, len(projectIDs))
	for _, id := range projectIDs {
		aris = append(aris, ariFromProjectID(cloudID, id))
	}
	return ruleDoc{
		Name:          name,
		Description:   description,
		State:         state,
		Trigger:       trigger,
		Components:    components,
		RuleScopeARIs: aris,
		Actor:         actorFromAccountID(actorAccountID),
	}, nil
}

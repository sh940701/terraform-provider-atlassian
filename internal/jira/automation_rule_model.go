package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// Model layer for atlassian_jira_automation_rule: converts between the
// Terraform shape and the Automation Rule Management API document
// (ruleDoc, https://api.atlassian.com/automation/public/jira). Pure
// functions; the resource layer only moves values in and out of framework
// types.
//
// `body` (a RuleBodyValue string, see below) carries only the rule's
// trigger and components — the parts of the automation component graph
// this resource does not model. Everything else the API needs (scope,
// actor, name/description/state) has its own attribute and is assembled
// into the full ruleDoc by docFromRule.

// ruleDoc is the Automation Rule Management API document: the payload of
// POST /rule (wrapped as {"rule": ruleDoc}) and the shape GET
// /rule/{ruleUuid} returns at the top level (verified against a real site
// for GET; the POST response shape is unconfirmed — see
// automation_rule_resource.go's createRuleResponse). PUT /rule/{ruleUuid}
// (Update) is assumed to accept the same shape, wrapped the same way as
// Create (see updateRuleRequest) — unconfirmed against a real site (T6); a
// follow-up task should verify.
type ruleDoc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	State       string          `json:"state"`
	Trigger     json.RawMessage `json:"trigger"`
	Components  json.RawMessage `json:"components"`
	// RuleScopeARIs is set on write from the union of project_ids (as
	// project ARIs) and extra_scope_aris (opaque, carried through
	// unchanged) — see scopeARIsFromProjectIDs.
	RuleScopeARIs []string   `json:"ruleScopeARIs"`
	Actor         *ruleActor `json:"actor,omitempty"`
	// AuthorAccountID is required by the server to even parse a write
	// request (real-site bisect 2026-09-21: omitting it yields 400 "The
	// request body could not be parsed"). Filled from actor_account_id, or
	// the requesting user (/rest/api/3/myself) when no actor is configured.
	AuthorAccountID string `json:"authorAccountId,omitempty"`
	// WriteAccessType is required by the server to parse a write request
	// (real-site bisect 2026-09-21). Always sent as UNRESTRICTED — the
	// resource does not model per-user write access.
	WriteAccessType     string `json:"writeAccessType,omitempty"`
	CanOtherRuleTrigger bool   `json:"canOtherRuleTrigger"`
	NotifyOnError       string `json:"notifyOnError,omitempty"`
	UUID                string `json:"uuid,omitempty"`
}

// ruleActor names who a rule's actions run as. Verified against a real
// site (2026-09-21, bsgglobal.atlassian.net): the API's shape is
// {"type": "ACCOUNT_ID", "actor": "<accountId>"} — the id lives under
// "actor", not "value". Sending "value" makes POST /rule answer 400
// "The request body could not be parsed". Kept behind actorFromAccountID and
// (*ruleActor).accountID so the shape stays in one place.
type ruleActor struct {
	Type  string `json:"type"`
	Value string `json:"actor"`
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
	trigger, err = withSchemaVersion(parsed.Trigger)
	if err != nil {
		return nil, nil, err
	}
	components, err = withSchemaVersion(parsed.Components)
	if err != nil {
		return nil, nil, err
	}
	return trigger, components, nil
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

// extraScopeARIsFromARIs extracts the non-project-scope ARIs from
// ruleScopeARIs, in order. These are scopes (e.g. a board or filter) that
// project_ids does not model; this resource surfaces them read-only via the
// extra_scope_aris attribute and carries them through unchanged on write
// (see scopeARIsFromProjectIDs) rather than silently dropping them.
func extraScopeARIsFromARIs(aris []string) []string {
	extra := make([]string, 0, len(aris))
	for _, ari := range aris {
		if _, ok := projectIDFromARI(ari); !ok {
			extra = append(extra, ari)
		}
	}
	return extra
}

// scopeARIsFromProjectIDs builds the full ruleScopeARIs value to send on
// write: project ARIs for projectIDs (under cloudID) followed by
// extraScopeARIs unchanged.
func scopeARIsFromProjectIDs(cloudID string, projectIDs, extraScopeARIs []string) []string {
	aris := make([]string, 0, len(projectIDs)+len(extraScopeARIs))
	for _, id := range projectIDs {
		aris = append(aris, ariFromProjectID(cloudID, id))
	}
	aris = append(aris, extraScopeARIs...)
	return aris
}

// docFromRule assembles the request document for a rule named name, scoped
// to cloudID's projectIDs plus extraScopeARIs (opaque non-project scopes
// carried through unchanged — pass nil on Create, where there are none
// yet), with the given body (trigger+components), optional actor account
// id, and the canOtherRuleTrigger/notifyOnError flags. Used for both Create
// (POST /rule) and Update (PUT /rule/{uuid}) — Update additionally sets the
// returned doc's UUID field itself, since docFromRule has no id to assign
// on Create.
func docFromRule(cloudID, name, description, state string, projectIDs, extraScopeARIs []string, body, actorAccountID, authorAccountID string, canOtherRuleTrigger bool, notifyOnError string) (ruleDoc, error) {
	trigger, components, err := docFromBody(body)
	if err != nil {
		return ruleDoc{}, err
	}
	return ruleDoc{
		Name:                name,
		Description:         description,
		State:               state,
		Trigger:             trigger,
		Components:          components,
		RuleScopeARIs:       scopeARIsFromProjectIDs(cloudID, projectIDs, extraScopeARIs),
		Actor:               actorFromAccountID(actorAccountID),
		AuthorAccountID:     authorAccountID,
		WriteAccessType:     ruleWriteAccessUnrestricted,
		CanOtherRuleTrigger: canOtherRuleTrigger,
		NotifyOnError:       notifyOnError,
	}, nil
}

const ruleWriteAccessUnrestricted = "UNRESTRICTED"

// withSchemaVersion returns raw with "schemaVersion": 1 added to every
// component object (trigger, components, and their children/conditions)
// that lacks one. The server refuses to parse a write request whose
// components carry no schemaVersion (real-site bisect 2026-09-21); it
// accepts 1 for every component type seen so far. normalizeBody strips
// the key for comparison, so this never shows up as drift.
func withSchemaVersion(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return raw, nil
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("parsing rule body for schemaVersion: %w", err)
	}
	var walk func(n interface{})
	walk = func(n interface{}) {
		switch t := n.(type) {
		case map[string]interface{}:
			if _, isComponent := t["component"]; isComponent {
				if _, ok := t["schemaVersion"]; !ok {
					t["schemaVersion"] = 1
				}
			}
			for _, k := range []string{"children", "conditions"} {
				if c, ok := t[k]; ok {
					walk(c)
				}
			}
		case []interface{}:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("re-encoding rule body: %w", err)
	}
	return out, nil
}

// normalizeBodyStrippedKeys are removed from every JSON object encountered
// during normalizeBody, regardless of nesting depth — keys the server adds
// to a rule document that were never in what the user wrote.
var normalizeBodyStrippedKeys = map[string]bool{
	"id":            true,
	"schemaVersion": true,
}

// normalizeBodyEmptyArrayKeys are dropped from a JSON object during
// normalizeBody when their value is an empty array — the server echoes
// these back to mean "none" even when the user's body never wrote them.
var normalizeBodyEmptyArrayKeys = map[string]bool{
	"conditions": true,
	"children":   true,
}

// normalizeBody normalizes raw for semantic COMPARISON only: it recursively
// removes normalizeBodyStrippedKeys, drops empty
// normalizeBodyEmptyArrayKeys arrays, sorts object keys, and re-marshals
// compactly. It must never be used to decide what gets sent on the wire —
// only whether two body values describe the same rule. (encoding/json
// already marshals Go maps with sorted keys, so building the normalized
// value as map[string]interface{} gets key sorting for free.)
func normalizeBody(raw json.RawMessage) (json.RawMessage, error) {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("parsing body for normalization: %w", err)
	}
	out, err := json.Marshal(normalizeBodyValue(v))
	if err != nil {
		return nil, fmt.Errorf("re-marshaling normalized body: %w", err)
	}
	return out, nil
}

func normalizeBodyValue(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(val))
		for k, child := range val {
			if normalizeBodyStrippedKeys[k] {
				continue
			}
			if normalizeBodyEmptyArrayKeys[k] {
				if arr, ok := child.([]interface{}); ok && len(arr) == 0 {
					continue
				}
			}
			out[k] = normalizeBodyValue(child)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(val))
		for i, child := range val {
			out[i] = normalizeBodyValue(child)
		}
		return out
	default:
		return val
	}
}

// bodyEqual reports whether a and b (each a `body` attribute JSON string)
// are semantically equal per normalizeBody.
func bodyEqual(a, b string) (bool, error) {
	na, err := normalizeBody(json.RawMessage(a))
	if err != nil {
		return false, fmt.Errorf("normalizing first body: %w", err)
	}
	nb, err := normalizeBody(json.RawMessage(b))
	if err != nil {
		return false, fmt.Errorf("normalizing second body: %w", err)
	}
	return string(na) == string(nb), nil
}

// bodyForState decides what Read should store as the `body` attribute:
// newBody (the server's re-serialized trigger+components) if it is not
// semantically equal to oldBody, or oldBody unchanged if it is equal — so a
// server echo that only adds "id"/"schemaVersion"/empty
// "conditions"/"children" (see normalizeBody) does not disturb the user's
// original formatting/ordering in state. oldBody == "" (no prior state,
// e.g. right after Create) always takes newBody.
func bodyForState(oldBody, newBody string) (string, error) {
	if oldBody == "" {
		return newBody, nil
	}
	equal, err := bodyEqual(oldBody, newBody)
	if err != nil {
		return "", err
	}
	if equal {
		return oldBody, nil
	}
	return newBody, nil
}

// RuleBodyType is the CustomType for the `body` attribute: a JSON string
// (RFC 7159) like jsontypes.NormalizedType, but with semantic equality
// defined by normalizeBody (ignoring server-added "id"/"schemaVersion" and
// empty "conditions"/"children" arrays) instead of plain whitespace/key
// -order normalization.
type RuleBodyType struct {
	basetypes.StringType
}

var _ basetypes.StringTypable = RuleBodyType{}

// String returns a human readable string of the type name.
func (t RuleBodyType) String() string {
	return "jira.RuleBodyType"
}

// ValueType returns the Value type.
func (t RuleBodyType) ValueType(_ context.Context) attr.Value {
	return RuleBodyValue{}
}

// Equal returns true if the given type is equivalent.
func (t RuleBodyType) Equal(o attr.Type) bool {
	other, ok := o.(RuleBodyType)
	if !ok {
		return false
	}
	return t.StringType.Equal(other.StringType)
}

// ValueFromString returns a StringValuable type given a StringValue.
func (t RuleBodyType) ValueFromString(_ context.Context, in basetypes.StringValue) (basetypes.StringValuable, diag.Diagnostics) {
	return RuleBodyValue{Normalized: jsontypes.Normalized{StringValue: in}}, nil
}

// ValueFromTerraform returns a Value given a tftypes.Value. Mirrors
// jsontypes.NormalizedType.ValueFromTerraform, routed through this type's
// own ValueFromString so the result is a RuleBodyValue.
func (t RuleBodyType) ValueFromTerraform(ctx context.Context, in tftypes.Value) (attr.Value, error) {
	attrValue, err := t.StringType.ValueFromTerraform(ctx, in)
	if err != nil {
		return nil, err
	}

	stringValue, ok := attrValue.(basetypes.StringValue)
	if !ok {
		return nil, fmt.Errorf("unexpected value type of %T", attrValue)
	}

	stringValuable, diags := t.ValueFromString(ctx, stringValue)
	if diags.HasError() {
		return nil, fmt.Errorf("unexpected error converting StringValue to StringValuable: %v", diags)
	}

	return stringValuable, nil
}

// RuleBodyValue is the attr.Value for the `body` attribute. It embeds
// jsontypes.Normalized for JSON-validity checking (xattr.ValidateableAttribute)
// and json.Unmarshal (promoted as-is, since neither depends on the concrete
// type), but overrides Type/Equal/StringSemanticEquals so that:
//   - its Type() is RuleBodyType (not jsontypes.NormalizedType);
//   - Equal() compares two RuleBodyValues (the promoted jsontypes.Normalized.Equal
//     would otherwise always return false here, since it type-asserts the
//     argument to jsontypes.Normalized, which a RuleBodyValue never is even
//     though it embeds one);
//   - StringSemanticEquals() uses normalizeBody instead of plain
//     whitespace/key-order JSON equivalence.
type RuleBodyValue struct {
	jsontypes.Normalized
}

var (
	_ basetypes.StringValuable                   = RuleBodyValue{}
	_ basetypes.StringValuableWithSemanticEquals = RuleBodyValue{}
)

// NewRuleBodyValue creates a RuleBodyValue with a known value.
func NewRuleBodyValue(value string) RuleBodyValue {
	return RuleBodyValue{Normalized: jsontypes.NewNormalizedValue(value)}
}

// NewRuleBodyNull creates a RuleBodyValue with a null value.
func NewRuleBodyNull() RuleBodyValue {
	return RuleBodyValue{Normalized: jsontypes.NewNormalizedNull()}
}

// NewRuleBodyUnknown creates a RuleBodyValue with an unknown value.
func NewRuleBodyUnknown() RuleBodyValue {
	return RuleBodyValue{Normalized: jsontypes.NewNormalizedUnknown()}
}

// Type returns a RuleBodyType.
func (v RuleBodyValue) Type(_ context.Context) attr.Type {
	return RuleBodyType{}
}

// Equal returns true if the given value is equivalent.
func (v RuleBodyValue) Equal(o attr.Value) bool {
	other, ok := o.(RuleBodyValue)
	if !ok {
		return false
	}
	return v.Normalized.Equal(other.Normalized)
}

// StringSemanticEquals returns true if newValuable is semantically equal to
// v per normalizeBody — i.e. ignoring server-added "id"/"schemaVersion" and
// empty "conditions"/"children" arrays, on top of the usual
// whitespace/key-order insensitivity.
func (v RuleBodyValue) StringSemanticEquals(_ context.Context, newValuable basetypes.StringValuable) (bool, diag.Diagnostics) {
	var diags diag.Diagnostics

	newValue, ok := newValuable.(RuleBodyValue)
	if !ok {
		diags.AddError(
			"Semantic Equality Check Error",
			"An unexpected value type was received while performing semantic equality checks. "+
				"Please report this to the provider developers.\n\n"+
				"Expected Value Type: "+fmt.Sprintf("%T", v)+"\n"+
				"Got Value Type: "+fmt.Sprintf("%T", newValuable),
		)
		return false, diags
	}

	equal, err := bodyEqual(v.ValueString(), newValue.ValueString())
	if err != nil {
		diags.AddError(
			"Semantic Equality Check Error",
			"An unexpected error occurred while performing semantic equality checks. "+
				"Please report this to the provider developers.\n\n"+
				"Error: "+err.Error(),
		)
		return false, diags
	}

	return equal, diags
}

package jira

import (
	"fmt"
	"strconv"

	"strings"
)

// Model layer for atlassian_jira_workflow: converts between the Terraform
// shape (workflowSpec — statuses by id, transitions with "who may move it")
// and the versioned-API document (jiraWorkflow). Pure functions; the
// resource layer only moves values in and out of framework types.
//
// Managed rules (everything else on a transition is preserved verbatim):
//
//	allowed_groups / allowed_roles / allowed_account_ids → system:restrict-issue-transition (one rule, 7 comma-string keys)
//	separation_of_duties[]                                → system:separation-of-duties (one rule per pair)
//	assign                                                → system:change-assignee (one post function)
//	required_fields[]                                     → system:validate-field-value ruleType=fieldRequired (one validator per field)

const (
	ruleKeyRestrict      = "system:restrict-issue-transition"
	ruleKeySoD           = "system:separation-of-duties"
	ruleKeyAssign        = "system:change-assignee"
	ruleKeyValidateField = "system:validate-field-value"

	initialTransitionName = "Create"

	transitionTypeInitial  = "INITIAL"
	transitionTypeGlobal   = "GLOBAL"
	transitionTypeDirected = "DIRECTED"
)

// restrictParamKeys are the seven parameters Jira expects on
// system:restrict-issue-transition, all present, comma-separated, "" if unused.
var restrictParamKeys = []string{
	"accountIds", "roleIds", "groupIds", "permissionKeys",
	"groupCustomFields", "allowUserCustomFields", "denyUserCustomFields",
}

// assignTypes are the change-assignee `type` values accepted by the
// validation endpoint on 2026-09-15 plus the official list.
var assignTypes = map[string]bool{
	"to-selected-user": true, "to-reporter": true, "to-current-user": true,
	"to-lead": true, "to-unassigned": true, "to-default-user": true,
}

type workflowSpec struct {
	Name        string
	Description string
	StatusIDs   []string // order = layout order; first = initial status
	Transitions []workflowTransitionSpec
}

type workflowTransitionSpec struct {
	Name               string
	Type               string // DIRECTED | GLOBAL
	From               []string
	To                 string
	AllowedGroups      []string
	AllowedRoles       []string
	AllowedAccountIDs  []string
	SeparationOfDuties []workflowSoDSpec
	Assign             *workflowAssignSpec
	RequiredFields     []string
}

type workflowSoDSpec struct {
	From string
	To   string
}

type workflowAssignSpec struct {
	Type      string
	AccountID string
}

// validateSpec checks what the API would reject only after a round trip.
func validateSpec(s workflowSpec) error {
	if len(s.StatusIDs) == 0 {
		return fmt.Errorf("at least one status is required")
	}
	known := map[string]bool{}
	for _, id := range s.StatusIDs {
		if id == "" {
			return fmt.Errorf("status_id must not be empty")
		}
		if known[id] {
			return fmt.Errorf("status %q listed twice", id)
		}
		known[id] = true
	}
	names := map[string]bool{}
	for _, t := range s.Transitions {
		if t.Name == "" {
			return fmt.Errorf("transition name must not be empty")
		}
		if t.Name == initialTransitionName {
			return fmt.Errorf("transition name %q is reserved for the initial transition", initialTransitionName)
		}
		if names[t.Name] {
			return fmt.Errorf("transition name %q is used twice (names must be unique)", t.Name)
		}
		names[t.Name] = true
		switch t.Type {
		case transitionTypeDirected:
			if len(t.From) == 0 {
				return fmt.Errorf("transition %q: DIRECTED transitions need at least one `from` status", t.Name)
			}
		case transitionTypeGlobal:
			if len(t.From) != 0 {
				return fmt.Errorf("transition %q: GLOBAL transitions must not set `from`", t.Name)
			}
		default:
			return fmt.Errorf("transition %q: type must be DIRECTED or GLOBAL, got %q", t.Name, t.Type)
		}
		if !known[t.To] {
			return fmt.Errorf("transition %q: `to` status %q is not in statuses", t.Name, t.To)
		}
		for _, f := range t.From {
			if !known[f] {
				return fmt.Errorf("transition %q: `from` status %q is not in statuses", t.Name, f)
			}
		}
		seenPair := map[[2]string]bool{}
		for _, p := range t.SeparationOfDuties {
			if !known[p.From] || !known[p.To] {
				return fmt.Errorf("transition %q: separation_of_duties refers to a status not in statuses (%q → %q)", t.Name, p.From, p.To)
			}
			if seenPair[[2]string{p.From, p.To}] {
				return fmt.Errorf("transition %q: separation_of_duties pair %q → %q listed twice", t.Name, p.From, p.To)
			}
			seenPair[[2]string{p.From, p.To}] = true
		}
		if t.Assign != nil {
			if !assignTypes[t.Assign.Type] {
				return fmt.Errorf("transition %q: assign.type %q is not supported", t.Name, t.Assign.Type)
			}
			if t.Assign.Type == "to-selected-user" && t.Assign.AccountID == "" {
				return fmt.Errorf("transition %q: assign.type to-selected-user requires account_id", t.Name)
			}
			if t.Assign.Type != "to-selected-user" && t.Assign.AccountID != "" {
				return fmt.Errorf("transition %q: assign.account_id is only valid with type to-selected-user", t.Name)
			}
		}
		seenField := map[string]bool{}
		for _, f := range t.RequiredFields {
			if f == "" {
				return fmt.Errorf("transition %q: required_fields must not contain empty strings", t.Name)
			}
			if seenField[f] {
				return fmt.Errorf("transition %q: required field %q listed twice", t.Name, f)
			}
			seenField[f] = true
		}
	}
	return nil
}

// statusRefIndex maps statusReference → status id from the top-level
// statuses array of a read/create/update response. Every entry must carry
// an id; otherwise we cannot express state in terms of status ids.
func statusRefIndex(defs []workflowStatusDef) (map[string]string, error) {
	if len(defs) == 0 {
		return nil, fmt.Errorf("workflow response carries no status definitions")
	}
	idx := make(map[string]string, len(defs))
	for _, d := range defs {
		if d.ID == "" || d.StatusReference == "" {
			return nil, fmt.Errorf("status definition without id/statusReference: %+v", d)
		}
		idx[d.StatusReference] = d.ID
	}
	return idx, nil
}

// refOf returns the document reference to use for a status id: the
// reference the server already uses when known, else the numeric id
// (accepted by the validation endpoint for existing global statuses).
func refOf(defs map[string]workflowStatusDef, id string) string {
	if d, ok := defs[id]; ok && d.StatusReference != "" {
		return d.StatusReference
	}
	return id
}

// buildCreateRequest assembles POST /rest/api/3/workflows/create for a
// GLOBAL (company-managed) workflow made of existing statuses.
func buildCreateRequest(spec workflowSpec, defs map[string]workflowStatusDef) (workflowCreateRequest, error) {
	if err := validateSpec(spec); err != nil {
		return workflowCreateRequest{}, err
	}
	topLevel := make([]workflowStatusDef, 0, len(spec.StatusIDs))
	for _, id := range spec.StatusIDs {
		d, ok := defs[id]
		if !ok {
			return workflowCreateRequest{}, fmt.Errorf("status %q not found", id)
		}
		topLevel = append(topLevel, workflowStatusDef{
			ID: id, StatusReference: refOf(defs, id), Name: d.Name, StatusCategory: d.StatusCategory,
		})
	}
	doc, err := buildDocument(spec, defs, nil)
	if err != nil {
		return workflowCreateRequest{}, err
	}
	return workflowCreateRequest{
		Scope:     workflowScope{Type: "GLOBAL"},
		Statuses:  topLevel,
		Workflows: []jiraWorkflow{doc},
	}, nil
}

// buildUpdateItem assembles one entry of POST /rest/api/3/workflows/update:
// the current server document with the spec merged in, so that transition
// ids, rule ids, layouts and rules we do not manage survive.
func buildUpdateItem(spec workflowSpec, current jiraWorkflow, defs map[string]workflowStatusDef) (workflowUpdateItem, error) {
	if err := validateSpec(spec); err != nil {
		return workflowUpdateItem{}, err
	}
	if current.Version == nil {
		return workflowUpdateItem{}, fmt.Errorf("current workflow document has no version")
	}
	doc, err := buildDocument(spec, defs, &current)
	if err != nil {
		return workflowUpdateItem{}, err
	}
	doc.ID = current.ID
	doc.Version = current.Version
	return workflowUpdateItem{jiraWorkflow: doc}, nil
}

// buildDocument builds the workflow document. When current is non-nil,
// server-side details are carried over by status reference / transition name.
func buildDocument(spec workflowSpec, defs map[string]workflowStatusDef, current *jiraWorkflow) (jiraWorkflow, error) {
	doc := jiraWorkflow{Name: spec.Name, Description: spec.Description}

	var curStatuses map[string]workflowStatusRef
	var curTransitions map[string]workflowTransition
	if current != nil {
		doc.StartPointLayout = current.StartPointLayout
		doc.LoopedTransitionContainerLayout = current.LoopedTransitionContainerLayout
		curStatuses = make(map[string]workflowStatusRef, len(current.Statuses))
		for _, s := range current.Statuses {
			curStatuses[s.StatusReference] = s
		}
		curTransitions = make(map[string]workflowTransition, len(current.Transitions))
		for _, t := range current.Transitions {
			curTransitions[t.Name] = t
		}
	}

	// Statuses in spec order; layouts/properties carried from the server.
	doc.Statuses = make([]workflowStatusRef, 0, len(spec.StatusIDs))
	for _, id := range spec.StatusIDs {
		ref := refOf(defs, id)
		if cur, ok := curStatuses[ref]; ok {
			doc.Statuses = append(doc.Statuses, cur)
			continue
		}
		doc.Statuses = append(doc.Statuses, workflowStatusRef{StatusReference: ref, Properties: map[string]string{}})
	}

	// INITIAL first — reuse the server's if present, retarget to the first status.
	initial := workflowTransition{Name: initialTransitionName, Type: transitionTypeInitial}
	if current != nil {
		for _, t := range current.Transitions {
			if t.Type == transitionTypeInitial {
				initial = t
				break
			}
		}
	}
	initial.ToStatusReference = refOf(defs, spec.StatusIDs[0])
	initial.Links = nil
	initial.Conditions = nil // the API rejects conditions on INITIAL
	doc.Transitions = append(doc.Transitions, initial)

	for _, ts := range spec.Transitions {
		var base workflowTransition
		if cur, ok := curTransitions[ts.Name]; ok {
			base = cur
		}
		doc.Transitions = append(doc.Transitions, mergeTransition(ts, base, defs))
	}
	assignTransitionIDs(doc.Transitions)
	return doc, nil
}

// assignTransitionIDs gives every transition without an id a fresh one. The
// create and update APIs require `transitions[].id` (real site: 400 "Missing
// required field 'payload.workflows.[0].transitions.[0].id'") and do not
// assign them. Existing ids are kept; new ones follow the Jira UI convention
// of the INITIAL transition being "1" and the rest counting up by 10 above
// the largest numeric id already present.
func assignTransitionIDs(transitions []workflowTransition) {
	next := 1
	used := map[string]bool{}
	for _, t := range transitions {
		if t.ID == "" {
			continue
		}
		used[t.ID] = true
		if n, err := strconv.Atoi(t.ID); err == nil && n >= next {
			next = n + 10
		}
	}
	for i := range transitions {
		if transitions[i].ID != "" {
			continue
		}
		if transitions[i].Type == transitionTypeInitial && !used["1"] {
			transitions[i].ID = "1"
			used["1"] = true
			if next == 1 {
				next = 11
			}
			continue
		}
		for used[strconv.Itoa(next)] {
			next += 10
		}
		transitions[i].ID = strconv.Itoa(next)
		used[transitions[i].ID] = true
		next += 10
	}
}

// mergeTransition writes the managed parts of ts over base (a server
// transition with the same name, or the zero value for a new one).
func mergeTransition(ts workflowTransitionSpec, base workflowTransition, defs map[string]workflowStatusDef) workflowTransition {
	t := base
	t.Name = ts.Name
	t.Type = ts.Type
	t.ToStatusReference = refOf(defs, ts.To)

	// Links: one per `from`; keep the server's port numbers when the same edge existed.
	existingLinks := map[string]workflowTransitionLink{}
	for _, l := range base.Links {
		existingLinks[l.FromStatusReference] = l
	}
	t.Links = nil
	for _, from := range ts.From {
		ref := refOf(defs, from)
		if l, ok := existingLinks[ref]; ok {
			t.Links = append(t.Links, l)
			continue
		}
		t.Links = append(t.Links, workflowTransitionLink{FromStatusReference: ref, FromPort: 0, ToPort: 1})
	}

	t.Conditions = mergeConditions(ts, base.Conditions, defs)
	t.Actions = mergeActions(ts, base.Actions)
	t.Validators = mergeValidators(ts, base.Validators)
	return t
}

func restrictParams(ts workflowTransitionSpec) (map[string]string, bool) {
	if len(ts.AllowedGroups)+len(ts.AllowedRoles)+len(ts.AllowedAccountIDs) == 0 {
		return nil, false
	}
	p := make(map[string]string, len(restrictParamKeys))
	for _, k := range restrictParamKeys {
		p[k] = ""
	}
	p["groupIds"] = strings.Join(ts.AllowedGroups, ",")
	p["roleIds"] = strings.Join(ts.AllowedRoles, ",")
	p["accountIds"] = strings.Join(ts.AllowedAccountIDs, ",")
	return p, true
}

// mergeConditions rebuilds the condition group: managed rules are updated
// in place (keeping ids and order), unmanaged rules are kept, removed
// managed rules disappear, new ones are appended.
func mergeConditions(ts workflowTransitionSpec, base *workflowConditionGroup, defs map[string]workflowStatusDef) *workflowConditionGroup {
	wantRestrict, hasRestrict := restrictParams(ts)
	wantSoD := map[[2]string]bool{}
	for _, p := range ts.SeparationOfDuties {
		wantSoD[[2]string{refOf(defs, p.From), refOf(defs, p.To)}] = true
	}
	doneRestrict := false
	doneSoD := map[[2]string]bool{}

	group := workflowConditionGroup{Operation: "ALL"}
	if base != nil {
		group.Operation = base.Operation
		group.ConditionGroups = base.ConditionGroups
		for _, c := range base.Conditions {
			switch c.RuleKey {
			case ruleKeyRestrict:
				if hasRestrict && !doneRestrict {
					group.Conditions = append(group.Conditions, workflowRule{ID: c.ID, RuleKey: ruleKeyRestrict, Parameters: wantRestrict})
					doneRestrict = true
				}
			case ruleKeySoD:
				pair := [2]string{c.Parameters["fromStatusId"], c.Parameters["toStatusId"]}
				if wantSoD[pair] && !doneSoD[pair] {
					group.Conditions = append(group.Conditions, c)
					doneSoD[pair] = true
				}
			default:
				group.Conditions = append(group.Conditions, c)
			}
		}
	}
	if hasRestrict && !doneRestrict {
		group.Conditions = append(group.Conditions, workflowRule{RuleKey: ruleKeyRestrict, Parameters: wantRestrict})
	}
	for _, p := range ts.SeparationOfDuties {
		pair := [2]string{refOf(defs, p.From), refOf(defs, p.To)}
		if doneSoD[pair] {
			continue
		}
		group.Conditions = append(group.Conditions, workflowRule{RuleKey: ruleKeySoD,
			Parameters: map[string]string{"fromStatusId": pair[0], "toStatusId": pair[1]}})
		doneSoD[pair] = true
	}
	if base == nil && len(group.Conditions) == 0 {
		return nil // omit the key entirely for a fresh transition without conditions
	}
	return &group
}

func assignParams(a *workflowAssignSpec) map[string]string {
	if a == nil {
		return nil
	}
	p := map[string]string{"type": a.Type}
	if a.Type == "to-selected-user" {
		p["accountId"] = a.AccountID
	}
	return p
}

func mergeActions(ts workflowTransitionSpec, base []workflowRule) []workflowRule {
	want := assignParams(ts.Assign)
	done := false
	var out []workflowRule
	for _, a := range base {
		if a.RuleKey == ruleKeyAssign {
			if want != nil && !done {
				out = append(out, workflowRule{ID: a.ID, RuleKey: ruleKeyAssign, Parameters: want})
				done = true
			}
			continue
		}
		out = append(out, a)
	}
	if want != nil && !done {
		out = append(out, workflowRule{RuleKey: ruleKeyAssign, Parameters: want})
	}
	return out
}

func isRequiredFieldValidator(v workflowRule) bool {
	return v.RuleKey == ruleKeyValidateField && v.Parameters["ruleType"] == "fieldRequired"
}

func mergeValidators(ts workflowTransitionSpec, base []workflowRule) []workflowRule {
	want := map[string]bool{}
	for _, f := range ts.RequiredFields {
		want[f] = true
	}
	done := map[string]bool{}
	var out []workflowRule
	for _, v := range base {
		if isRequiredFieldValidator(v) {
			f := v.Parameters["fieldsRequired"]
			if want[f] && !done[f] {
				out = append(out, v)
				done[f] = true
			}
			continue
		}
		out = append(out, v)
	}
	for _, f := range ts.RequiredFields {
		if done[f] {
			continue
		}
		out = append(out, workflowRule{RuleKey: ruleKeyValidateField, Parameters: map[string]string{
			"ruleType": "fieldRequired", "fieldsRequired": f, "ignoreContext": "true",
			"errorMessage": fmt.Sprintf("%s 필수", f),
		}})
		done[f] = true
	}
	return out
}

// specFromDocument reads the managed shape back out of a server document.
// refToID comes from statusRefIndex on the response's top-level statuses.
func specFromDocument(doc jiraWorkflow, refToID map[string]string) (workflowSpec, error) {
	idOf := func(ref string) (string, error) {
		id, ok := refToID[ref]
		if !ok {
			return "", fmt.Errorf("workflow %q references status %q which is not in the response's status list", doc.Name, ref)
		}
		return id, nil
	}
	spec := workflowSpec{Name: doc.Name, Description: doc.Description}
	for _, s := range doc.Statuses {
		id, err := idOf(s.StatusReference)
		if err != nil {
			return workflowSpec{}, err
		}
		spec.StatusIDs = append(spec.StatusIDs, id)
	}
	for _, t := range doc.Transitions {
		if t.Type == transitionTypeInitial {
			continue
		}
		ts := workflowTransitionSpec{Name: t.Name, Type: t.Type}
		to, err := idOf(t.ToStatusReference)
		if err != nil {
			return workflowSpec{}, err
		}
		ts.To = to
		for _, l := range t.Links {
			from, err := idOf(l.FromStatusReference)
			if err != nil {
				return workflowSpec{}, err
			}
			ts.From = append(ts.From, from)
		}
		if t.Conditions != nil {
			for _, c := range t.Conditions.Conditions {
				switch c.RuleKey {
				case ruleKeyRestrict:
					ts.AllowedGroups = splitCSV(c.Parameters["groupIds"])
					ts.AllowedRoles = splitCSV(c.Parameters["roleIds"])
					ts.AllowedAccountIDs = splitCSV(c.Parameters["accountIds"])
				case ruleKeySoD:
					from, err := idOf(c.Parameters["fromStatusId"])
					if err != nil {
						return workflowSpec{}, err
					}
					to, err := idOf(c.Parameters["toStatusId"])
					if err != nil {
						return workflowSpec{}, err
					}
					ts.SeparationOfDuties = append(ts.SeparationOfDuties, workflowSoDSpec{From: from, To: to})
				}
			}
		}
		for _, a := range t.Actions {
			if a.RuleKey == ruleKeyAssign && ts.Assign == nil {
				ts.Assign = &workflowAssignSpec{Type: a.Parameters["type"], AccountID: a.Parameters["accountId"]}
			}
		}
		for _, v := range t.Validators {
			if isRequiredFieldValidator(v) {
				ts.RequiredFields = append(ts.RequiredFields, v.Parameters["fieldsRequired"])
			}
		}
		spec.Transitions = append(spec.Transitions, ts)
	}
	return spec, nil
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// orderTransitionsLike re-orders transitions to follow prior (a list of
// transition names, e.g. from state or config): the API returns them in its
// own order (GLOBAL first, then INITIAL, then the rest), which would show up
// as a spurious diff. Names not in prior keep their server order at the end.
func orderTransitionsLike(transitions []workflowTransitionSpec, prior []string) []workflowTransitionSpec {
	if len(transitions) == 0 {
		return nil
	}
	byName := make(map[string]int, len(transitions))
	for i, t := range transitions {
		byName[t.Name] = i
	}
	out := make([]workflowTransitionSpec, 0, len(transitions))
	taken := make([]bool, len(transitions))
	for _, name := range prior {
		if i, ok := byName[name]; ok && !taken[i] {
			out = append(out, transitions[i])
			taken[i] = true
		}
	}
	for i, t := range transitions {
		if !taken[i] {
			out = append(out, t)
		}
	}
	return out
}

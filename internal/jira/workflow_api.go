package jira

import "encoding/json"

// Wire types for the versioned Jira Cloud workflow API:
//
//   POST /rest/api/3/workflows              bulk get   → workflowReadResponse
//   POST /rest/api/3/workflows/create       create     ← workflowCreateRequest → workflowCreateResponse
//   POST /rest/api/3/workflows/update       update     ← workflowUpdateRequest → workflowUpdateResponse
//   POST …/create/validation, …/update/validation      → workflowValidationResponse
//
// The API is full-document: an update replaces the whole workflow, so every
// field the server sends back must survive unmarshal → marshal unchanged.
// Fields we do not model as Terraform attributes (layouts, rule ids, unknown
// rules, triggers, transition screens) are therefore kept verbatim here and
// only reshaped at the resource layer. Pointer + omitempty marks fields that
// appear in responses but must not be invented in requests.

// workflowReadRequest is the body of POST /rest/api/3/workflows (bulk get).
type workflowReadRequest struct {
	WorkflowIDs   []string `json:"workflowIds,omitempty"`
	WorkflowNames []string `json:"workflowNames,omitempty"`
}

// workflowReadResponse is the response of bulk get / create / update.
type workflowReadResponse struct {
	Statuses  []workflowStatusDef `json:"statuses"`
	Workflows []jiraWorkflow      `json:"workflows"`
	// TaskID is set by update when the change is applied asynchronously.
	TaskID string `json:"taskId,omitempty"`
}

// workflowStatusDef is a top-level status definition: it binds a status
// reference used inside workflow documents to an existing (or new) status.
type workflowStatusDef struct {
	ID              string         `json:"id,omitempty"`
	StatusReference string         `json:"statusReference"`
	Name            string         `json:"name"`
	StatusCategory  string         `json:"statusCategory"`
	Description     *string        `json:"description,omitempty"`
	Scope           *workflowScope `json:"scope,omitempty"`
}

type workflowScope struct {
	Type    string             `json:"type"` // GLOBAL | PROJECT
	Project *workflowProjectID `json:"project,omitempty"`
}

type workflowProjectID struct {
	ID string `json:"id"`
}

type workflowLayout struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type documentVersion struct {
	ID            string `json:"id"`
	VersionNumber int    `json:"versionNumber"`
}

// jiraWorkflow is one workflow document (response shape). Create and update
// requests embed the same document with the fields the API expects.
type jiraWorkflow struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description"`

	Scope                           *workflowScope   `json:"scope,omitempty"`
	Version                         *documentVersion `json:"version,omitempty"`
	IsEditable                      *bool            `json:"isEditable,omitempty"`
	Created                         string           `json:"created,omitempty"`
	Updated                         string           `json:"updated,omitempty"`
	TaskID                          string           `json:"taskId,omitempty"`
	StartPointLayout                *workflowLayout  `json:"startPointLayout,omitempty"`
	LoopedTransitionContainerLayout *workflowLayout  `json:"loopedTransitionContainerLayout,omitempty"`

	Statuses    []workflowStatusRef  `json:"statuses"`
	Transitions []workflowTransition `json:"transitions"`
}

// workflowStatusRef is a status as placed inside a workflow document.
type workflowStatusRef struct {
	StatusReference       string            `json:"statusReference"`
	Layout                *workflowLayout   `json:"layout,omitempty"`
	Properties            map[string]string `json:"properties"`
	Deprecated            *bool             `json:"deprecated,omitempty"`
	ApprovalConfiguration json.RawMessage   `json:"approvalConfiguration,omitempty"`
}

type workflowTransitionLink struct {
	FromStatusReference string `json:"fromStatusReference"`
	FromPort            int    `json:"fromPort"`
	ToPort              int    `json:"toPort"`
}

// workflowRule is a condition / validator / post function (action) / trigger.
// Parameters are opaque key/value strings; their meaning depends on RuleKey.
type workflowRule struct {
	ID         string            `json:"id,omitempty"`
	RuleKey    string            `json:"ruleKey"`
	Parameters map[string]string `json:"parameters"`
}

type workflowConditionGroup struct {
	Operation       string                   `json:"operation"` // ALL | ANY
	ConditionGroups []workflowConditionGroup `json:"conditionGroups"`
	Conditions      []workflowRule           `json:"conditions"`
}

// workflowTransition is one transition of a workflow document.
type workflowTransition struct {
	ID                 string                   `json:"id,omitempty"`
	Name               string                   `json:"name"`
	Description        *string                  `json:"description,omitempty"`
	Type               string                   `json:"type"` // INITIAL | GLOBAL | DIRECTED
	ToStatusReference  string                   `json:"toStatusReference"`
	Links              []workflowTransitionLink `json:"links"`
	Conditions         *workflowConditionGroup  `json:"conditions,omitempty"`
	Actions            []workflowRule           `json:"actions"`
	Validators         []workflowRule           `json:"validators"`
	Triggers           []workflowRule           `json:"triggers"`
	Properties         map[string]string        `json:"properties"`
	CustomIssueEventID string                   `json:"customIssueEventId,omitempty"`
	TransitionScreen   *workflowRule            `json:"transitionScreen,omitempty"`
}

// MarshalJSON guarantees explicit empty collections. The API distinguishes
// "no rules" (`[]`) from a missing/null key, and Go would otherwise emit
// `null` for nil slices and maps on transitions we assemble ourselves.
func (t workflowTransition) MarshalJSON() ([]byte, error) {
	type alias workflowTransition
	a := alias(t)
	if a.Links == nil {
		a.Links = []workflowTransitionLink{}
	}
	if a.Actions == nil {
		a.Actions = []workflowRule{}
	}
	if a.Validators == nil {
		a.Validators = []workflowRule{}
	}
	if a.Triggers == nil {
		a.Triggers = []workflowRule{}
	}
	if a.Properties == nil {
		a.Properties = map[string]string{}
	}
	return json.Marshal(a)
}

// MarshalJSON keeps conditionGroups / conditions explicit for the same reason.
func (g workflowConditionGroup) MarshalJSON() ([]byte, error) {
	type alias workflowConditionGroup
	a := alias(g)
	if a.ConditionGroups == nil {
		a.ConditionGroups = []workflowConditionGroup{}
	}
	if a.Conditions == nil {
		a.Conditions = []workflowRule{}
	}
	return json.Marshal(a)
}

// MarshalJSON keeps status properties explicit (`{}`), which the API expects.
func (s workflowStatusRef) MarshalJSON() ([]byte, error) {
	type alias workflowStatusRef
	a := alias(s)
	if a.Properties == nil {
		a.Properties = map[string]string{}
	}
	return json.Marshal(a)
}

// workflowCreateRequest is the body of POST /rest/api/3/workflows/create.
type workflowCreateRequest struct {
	Scope     workflowScope       `json:"scope"`
	Statuses  []workflowStatusDef `json:"statuses"`
	Workflows []jiraWorkflow      `json:"workflows"`
}

// workflowUpdateRequest is the body of POST /rest/api/3/workflows/update.
// Each workflow carries its id and current version (optimistic lock).
type workflowUpdateRequest struct {
	Statuses  []workflowStatusDef  `json:"statuses"`
	Workflows []workflowUpdateItem `json:"workflows"`
}

type workflowUpdateItem struct {
	jiraWorkflow
	DefaultStatusMappings []workflowStatusMigration `json:"defaultStatusMappings"`
	StatusMappings        []workflowStatusMapping   `json:"statusMappings"`
}

// MarshalJSON flattens the embedded document and forces explicit empty
// mapping arrays (the API rejects a missing key with a validation error).
func (u workflowUpdateItem) MarshalJSON() ([]byte, error) {
	doc, err := json.Marshal(u.jiraWorkflow)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(doc, &m); err != nil {
		return nil, err
	}
	// WorkflowUpdate (swagger) = id, version, description, startPointLayout,
	// loopedTransitionContainerLayout, statuses, transitions, statusMappings,
	// defaultStatusMappings — never the read-only document fields.
	for _, k := range []string{"name", "scope", "isEditable", "created", "updated", "taskId"} {
		delete(m, k)
	}
	dsm := u.DefaultStatusMappings
	if dsm == nil {
		dsm = []workflowStatusMigration{}
	}
	sm := u.StatusMappings
	if sm == nil {
		sm = []workflowStatusMapping{}
	}
	m["defaultStatusMappings"], _ = json.Marshal(dsm)
	m["statusMappings"], _ = json.Marshal(sm)
	return json.Marshal(m)
}

type workflowStatusMigration struct {
	OldStatusReference string `json:"oldStatusReference"`
	NewStatusReference string `json:"newStatusReference"`
}

type workflowStatusMapping struct {
	IssueTypeID      string                    `json:"issueTypeId"`
	ProjectID        string                    `json:"projectId"`
	StatusMigrations []workflowStatusMigration `json:"statusMigrations"`
}

// workflowValidationRequest wraps a create or update body for the
// …/validation endpoints. Payload is the exact request that would be sent.
type workflowValidationRequest struct {
	Payload           interface{}               `json:"payload"`
	ValidationOptions workflowValidationOptions `json:"validationOptions"`
}

type workflowValidationOptions struct {
	Levels []string `json:"levels"` // ERROR, WARNING
}

type workflowValidationResponse struct {
	Errors []workflowValidationError `json:"errors"`
}

type workflowValidationError struct {
	Code             string                 `json:"code"`
	Level            string                 `json:"level"`
	Type             string                 `json:"type"`
	Message          string                 `json:"message"`
	ElementReference map[string]interface{} `json:"elementReference"`
}

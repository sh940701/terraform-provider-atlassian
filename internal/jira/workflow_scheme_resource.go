package jira

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

var (
	_ resource.Resource                = &workflowSchemeResource{}
	_ resource.ResourceWithImportState = &workflowSchemeResource{}
)

// NewWorkflowSchemeResource returns a new workflow scheme resource.
func NewWorkflowSchemeResource() resource.Resource {
	return &workflowSchemeResource{}
}

type workflowSchemeResource struct {
	client *atlassian.Client
}

type workflowSchemeResourceModel struct {
	ID                types.String `tfsdk:"id"`
	Name              types.String `tfsdk:"name"`
	Description       types.String `tfsdk:"description"`
	DefaultWorkflow   types.String `tfsdk:"default_workflow"`
	IssueTypeMappings types.Map    `tfsdk:"issue_type_mappings"`
}

// workflowSchemeAPIResponse represents the Jira workflow scheme API response shape.
type workflowSchemeAPIResponse struct {
	ID                int64             `json:"id"`
	Name              string            `json:"name"`
	Description       string            `json:"description"`
	DefaultWorkflow   string            `json:"defaultWorkflow"`
	IssueTypeMappings map[string]string `json:"issueTypeMappings"`
	Draft             bool              `json:"draft"`
}

// workflowSchemeCreateRequest represents the Jira workflow scheme create/update request body.
type workflowSchemeCreateRequest struct {
	Name              string            `json:"name"`
	Description       string            `json:"description,omitempty"`
	DefaultWorkflow   string            `json:"defaultWorkflow,omitempty"`
	IssueTypeMappings map[string]string `json:"issueTypeMappings,omitempty"`
}

func (r *workflowSchemeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_jira_workflow_scheme"
}

func (r *workflowSchemeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a Jira Cloud workflow scheme.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "The ID of the workflow scheme.",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Description: "The name of the workflow scheme.",
				Required:    true,
			},
			"description": schema.StringAttribute{
				Description: "The description of the workflow scheme.",
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(""),
			},
			"default_workflow": schema.StringAttribute{
				Description: "The name of the default workflow for the scheme.",
				Optional:    true,
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"issue_type_mappings": schema.MapAttribute{
				Description: "A map of issue type IDs to workflow names.",
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				PlanModifiers: []planmodifier.Map{
					mapplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *workflowSchemeResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	client, ok := req.ProviderData.(*atlassian.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Resource Configure Type",
			fmt.Sprintf("Expected *atlassian.Client, got: %T", req.ProviderData),
		)
		return
	}

	r.client = client
}

func (r *workflowSchemeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan workflowSchemeResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := workflowSchemeCreateRequest{
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
	}

	if !plan.DefaultWorkflow.IsNull() && !plan.DefaultWorkflow.IsUnknown() {
		body.DefaultWorkflow = plan.DefaultWorkflow.ValueString()
	}

	if !plan.IssueTypeMappings.IsNull() && !plan.IssueTypeMappings.IsUnknown() {
		mappings := make(map[string]string)
		resp.Diagnostics.Append(plan.IssueTypeMappings.ElementsAs(ctx, &mappings, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		body.IssueTypeMappings = mappings
	}

	var result workflowSchemeAPIResponse
	err := r.client.Post(ctx, "/rest/api/3/workflowscheme", body, &result)
	if err != nil {
		resp.Diagnostics.AddError("Error creating workflow scheme", err.Error())
		return
	}

	// Only the ID is definitively from the server; preserve plan values but
	// resolve any Unknown computed fields using the server response.
	plan.ID = types.StringValue(fmt.Sprintf("%d", result.ID))

	// Resolve Unknown computed values from the server response.
	if plan.DefaultWorkflow.IsUnknown() {
		plan.DefaultWorkflow = types.StringValue(result.DefaultWorkflow)
	}

	if plan.IssueTypeMappings.IsUnknown() {
		mappingsValue, diags := types.MapValueFrom(ctx, types.StringType, result.IssueTypeMappings)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		plan.IssueTypeMappings = mappingsValue
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *workflowSchemeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state workflowSchemeResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var result workflowSchemeAPIResponse
	apiPath := fmt.Sprintf("/rest/api/3/workflowscheme/%s", atlassian.PathEscape(state.ID.ValueString()))
	statusCode, err := r.client.GetWithStatus(ctx, apiPath, &result)
	if err != nil {
		resp.Diagnostics.AddError("Error reading workflow scheme", err.Error())
		return
	}

	if statusCode == http.StatusNotFound {
		resp.State.RemoveResource(ctx)
		return
	}

	state.ID = types.StringValue(fmt.Sprintf("%d", result.ID))
	state.Name = types.StringValue(result.Name)
	state.Description = types.StringValue(result.Description)
	state.DefaultWorkflow = types.StringValue(result.DefaultWorkflow)

	mappingsValue, diags := types.MapValueFrom(ctx, types.StringType, result.IssueTypeMappings)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.IssueTypeMappings = mappingsValue

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *workflowSchemeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan workflowSchemeResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var state workflowSchemeResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := workflowSchemeCreateRequest{
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
	}

	if !plan.DefaultWorkflow.IsNull() && !plan.DefaultWorkflow.IsUnknown() {
		body.DefaultWorkflow = plan.DefaultWorkflow.ValueString()
	}

	if !plan.IssueTypeMappings.IsNull() && !plan.IssueTypeMappings.IsUnknown() {
		mappings := make(map[string]string)
		resp.Diagnostics.Append(plan.IssueTypeMappings.ElementsAs(ctx, &mappings, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		body.IssueTypeMappings = mappings
	}

	var result workflowSchemeAPIResponse
	apiPath := fmt.Sprintf("/rest/api/3/workflowscheme/%s", atlassian.PathEscape(state.ID.ValueString()))
	status, err := r.client.PutWithStatus(ctx, apiPath, body, &result)
	if err != nil && status == http.StatusBadRequest {
		// An ACTIVE scheme (one a project uses) refuses direct mapping
		// changes — Jira answers 400 "Cannot change the mappings of an
		// active workflow scheme". The supported route is a draft: create
		// it, write the new mappings to it, publish it (async task), and
		// wait for the task. Seen on the first real apply that added issue
		// types to a live project (2026-09-21).
		if derr := r.updateViaDraft(ctx, state.ID.ValueString(), body, &result); derr != nil {
			resp.Diagnostics.AddError("Error updating workflow scheme",
				fmt.Sprintf("direct update failed (%s); draft update failed too: %s", err, derr))
			return
		}
		err = nil
	}
	if err != nil {
		resp.Diagnostics.AddError("Error updating workflow scheme", err.Error())
		return
	}

	// ID carries forward from state (unchanged on update); preserve all plan values
	// and resolve any Unknown computed fields using the server response.
	plan.ID = state.ID

	if plan.DefaultWorkflow.IsUnknown() {
		plan.DefaultWorkflow = types.StringValue(result.DefaultWorkflow)
	}

	if plan.IssueTypeMappings.IsUnknown() {
		mappingsValue, diags := types.MapValueFrom(ctx, types.StringType, result.IssueTypeMappings)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		plan.IssueTypeMappings = mappingsValue
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// workflowSchemeDraftRequest is the PUT /workflowscheme/{id}/draft body.
type workflowSchemeDraftRequest struct {
	Name                string            `json:"name"`
	Description         string            `json:"description,omitempty"`
	DefaultWorkflow     string            `json:"defaultWorkflow,omitempty"`
	IssueTypeMappings   map[string]string `json:"issueTypeMappings,omitempty"`
	UpdateDraftIfNeeded bool              `json:"updateDraftIfNeeded"`
}

// workflowSchemeTask is the TaskProgressBeanObject a draft publish returns
// (via the 303 the HTTP client follows) and GET /task/{id} reports.
type workflowSchemeTask struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// updateViaDraft applies body to an active workflow scheme through Jira's
// draft mechanism: create (or reuse) the draft, set its mappings, publish
// it, and wait for the publish task. Existing issues keep their statuses
// (no status mappings are sent — the caller only adds/changes mappings for
// issue types that have no issues yet, or whose statuses exist in the new
// workflow). result receives the scheme as read back after publishing.
func (r *workflowSchemeResource) updateViaDraft(ctx context.Context, id string, body workflowSchemeCreateRequest, result *workflowSchemeAPIResponse) error {
	base := fmt.Sprintf("/rest/api/3/workflowscheme/%s", atlassian.PathEscape(id))

	var draft workflowSchemeAPIResponse
	if status, err := r.client.PostWithStatus(ctx, base+"/createdraft", struct{}{}, &draft); err != nil {
		// A draft may already exist (e.g. a previous run died after
		// creating one); use it instead of failing.
		if status != http.StatusBadRequest {
			return fmt.Errorf("creating draft: %w", err)
		}
		if gerr := r.client.Get(ctx, base+"/draft", &draft); gerr != nil {
			return fmt.Errorf("creating draft: %w (and no existing draft: %s)", err, gerr)
		}
	}

	draftBody := workflowSchemeDraftRequest{
		Name: body.Name, Description: body.Description, DefaultWorkflow: body.DefaultWorkflow,
		IssueTypeMappings: body.IssueTypeMappings, UpdateDraftIfNeeded: true,
	}
	var written workflowSchemeDraftResponse
	if err := r.client.Put(ctx, base+"/draft", draftBody, &written); err != nil {
		return fmt.Errorf("writing draft mappings: %w", err)
	}

	// Publishing refuses issue types whose old workflow has statuses the new
	// one lacks ("Issue type with ID X is missing the mappings required for
	// statuses with IDs ...") — even when no issue of that type exists yet.
	// Ask Jira which (issue type, status) pairs need a mapping and send each
	// to the new workflow's initial status.
	statusMappings, err := r.requiredStatusMappings(ctx, id, written)
	if err != nil {
		return err
	}

	var task workflowSchemeTask
	if _, err := r.client.PostWithStatus(ctx, base+"/draft/publish", map[string]interface{}{"statusMappings": statusMappings}, &task); err != nil {
		return fmt.Errorf("publishing draft: %w", err)
	}
	if task.ID != "" {
		if err := r.waitForTask(ctx, task.ID); err != nil {
			return fmt.Errorf("publishing draft: %w", err)
		}
	}

	if err := r.client.Get(ctx, base, result); err != nil {
		return fmt.Errorf("reading scheme after publish: %w", err)
	}
	return nil
}

// workflowSchemeDraftResponse is what PUT/GET /workflowscheme/{id}/draft
// returns — the draft mappings plus the live ("original") ones.
type workflowSchemeDraftResponse struct {
	workflowSchemeAPIResponse
	OriginalDefaultWorkflow   string            `json:"originalDefaultWorkflow"`
	OriginalIssueTypeMappings map[string]string `json:"originalIssueTypeMappings"`
}

// statusMapping is one entry of PublishDraftWorkflowScheme.statusMappings.
type statusMapping struct {
	IssueTypeID string `json:"issueTypeId"`
	StatusID    string `json:"statusId"`
	NewStatusID string `json:"newStatusId"`
}

// requiredStatusMappings asks POST /workflowscheme/update/mappings which
// (issue type, status) pairs the draft's workflow changes leave unmapped and
// maps each to the initial status of that issue type's new workflow. Issue
// types the live scheme does not map explicitly are treated as moving off
// the live default workflow.
func (r *workflowSchemeResource) requiredStatusMappings(ctx context.Context, schemeID string, draft workflowSchemeDraftResponse) ([]statusMapping, error) {
	// issue type → new workflow name, only where the workflow actually changes
	changed := map[string]string{}
	for issueType, newWF := range draft.IssueTypeMappings {
		oldWF, ok := draft.OriginalIssueTypeMappings[issueType]
		if !ok {
			oldWF = draft.OriginalDefaultWorkflow
		}
		if oldWF != newWF {
			changed[issueType] = newWF
		}
	}
	if len(changed) == 0 {
		return []statusMapping{}, nil
	}

	// workflow name → id (the mappings endpoint speaks ids)
	wfID := map[string]string{}
	for _, name := range changed {
		if _, done := wfID[name]; done {
			continue
		}
		id, err := r.workflowIDByName(ctx, name)
		if err != nil {
			return nil, err
		}
		wfID[name] = id
	}

	byWF := map[string][]string{}
	for issueType, name := range changed {
		byWF[wfID[name]] = append(byWF[wfID[name]], issueType)
	}
	req := map[string]interface{}{"id": schemeID, "workflowsForIssueTypes": []map[string]interface{}{}}
	for id, issueTypes := range byWF {
		sort.Strings(issueTypes)
		req["workflowsForIssueTypes"] = append(req["workflowsForIssueTypes"].([]map[string]interface{}), map[string]interface{}{"workflowId": id, "issueTypeIds": issueTypes})
	}
	var required struct {
		ByIssueType []struct {
			IssueTypeID string   `json:"issueTypeId"`
			StatusIDs   []string `json:"statusIds"`
		} `json:"statusMappingsByIssueTypes"`
		ByWorkflow []struct {
			TargetWorkflowID string   `json:"targetWorkflowId"`
			StatusIDs        []string `json:"statusIds"`
		} `json:"statusMappingsByWorkflows"`
		PerWorkflow []struct {
			WorkflowID      string `json:"workflowId"`
			InitialStatusID string `json:"initialStatusId"`
		} `json:"statusesPerWorkflow"`
	}
	if err := r.client.Post(ctx, "/rest/api/3/workflowscheme/update/mappings", req, &required); err != nil {
		return nil, fmt.Errorf("asking Jira which status mappings the draft needs: %w", err)
	}
	initial := map[string]string{}
	for _, w := range required.PerWorkflow {
		initial[w.WorkflowID] = w.InitialStatusID
	}

	// (issue type, status) pairs to map; by-workflow entries apply to every
	// issue type moving to that workflow
	need := map[string]map[string]bool{}
	add := func(issueType, status string) {
		if need[issueType] == nil {
			need[issueType] = map[string]bool{}
		}
		need[issueType][status] = true
	}
	for _, e := range required.ByIssueType {
		for _, st := range e.StatusIDs {
			add(e.IssueTypeID, st)
		}
	}
	for _, e := range required.ByWorkflow {
		for issueType, name := range changed {
			if wfID[name] == e.TargetWorkflowID {
				for _, st := range e.StatusIDs {
					add(issueType, st)
				}
			}
		}
	}

	out := []statusMapping{}
	for issueType, statuses := range need {
		target := initial[wfID[changed[issueType]]]
		if target == "" {
			return nil, fmt.Errorf("issue type %s needs status mappings but Jira reported no initial status for workflow %q", issueType, changed[issueType])
		}
		for st := range statuses {
			out = append(out, statusMapping{IssueTypeID: issueType, StatusID: st, NewStatusID: target})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IssueTypeID != out[j].IssueTypeID {
			return out[i].IssueTypeID < out[j].IssueTypeID
		}
		return out[i].StatusID < out[j].StatusID
	})
	return out, nil
}

// workflowIDByName resolves a workflow's id through GET /workflows/search
// (exact name match; the query is a substring filter).
func (r *workflowSchemeResource) workflowIDByName(ctx context.Context, name string) (string, error) {
	var page struct {
		Values []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"values"`
	}
	if err := r.client.Get(ctx, "/rest/api/3/workflows/search?maxResults=50&queryString="+url.QueryEscape(name), &page); err != nil {
		return "", fmt.Errorf("looking up workflow %q: %w", name, err)
	}
	for _, w := range page.Values {
		if w.Name == name {
			return w.ID, nil
		}
	}
	return "", fmt.Errorf("workflow %q not found", name)
}

// waitForTask polls GET /rest/api/3/task/{id} until the task finishes.
func (r *workflowSchemeResource) waitForTask(ctx context.Context, taskID string) error {
	deadline := time.Now().Add(10 * time.Minute)
	for {
		var task workflowSchemeTask
		if err := r.client.Get(ctx, "/rest/api/3/task/"+atlassian.PathEscape(taskID), &task); err != nil {
			return fmt.Errorf("polling task %s: %w", taskID, err)
		}
		switch task.Status {
		case "COMPLETE":
			return nil
		case "FAILED", "CANCELLED", "DEAD":
			return fmt.Errorf("task %s ended with status %s: %s", taskID, task.Status, task.Message)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("task %s still %s after 10 minutes", taskID, task.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (r *workflowSchemeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state workflowSchemeResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiPath := fmt.Sprintf("/rest/api/3/workflowscheme/%s", atlassian.PathEscape(state.ID.ValueString()))
	statusCode, err := r.client.DeleteWithStatus(ctx, apiPath)
	if err != nil {
		resp.Diagnostics.AddError("Error deleting workflow scheme", err.Error())
		return
	}

	// 404 means the workflow scheme was already deleted out-of-band; treat as success.
	if statusCode == http.StatusNotFound {
		return
	}
}

func (r *workflowSchemeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

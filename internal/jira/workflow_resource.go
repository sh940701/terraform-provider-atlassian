package jira

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

var (
	_ resource.Resource                = &workflowResource{}
	_ resource.ResourceWithImportState = &workflowResource{}
)

// workflowConflictRetryDelay is how long to wait before the single retry
// after a 409 ("another workflow configuration update task is ongoing").
var workflowConflictRetryDelay = 3 * time.Second

// NewWorkflowResource returns a new workflow resource.
func NewWorkflowResource() resource.Resource {
	return &workflowResource{}
}

type workflowResource struct {
	client *atlassian.Client
}

// Terraform-facing models. Lists of nested objects are typed through the
// attr type maps below so they can be rebuilt from plain Go values in Read.

type workflowResourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	Statuses    types.List   `tfsdk:"statuses"`
	Transitions types.List   `tfsdk:"transitions"`
	Version     types.Int64  `tfsdk:"version"`
}

type workflowStatusModel struct {
	StatusID types.String `tfsdk:"status_id"`
}

type workflowTransitionModel struct {
	Name               types.String `tfsdk:"name"`
	Type               types.String `tfsdk:"type"`
	From               types.List   `tfsdk:"from"`
	To                 types.String `tfsdk:"to"`
	AllowedGroups      types.List   `tfsdk:"allowed_groups"`
	AllowedRoles       types.List   `tfsdk:"allowed_roles"`
	AllowedAccountIDs  types.List   `tfsdk:"allowed_account_ids"`
	SeparationOfDuties types.List   `tfsdk:"separation_of_duties"`
	Assign             types.Object `tfsdk:"assign"`
	RequiredFields     types.List   `tfsdk:"required_fields"`
}

type workflowSoDModel struct {
	From types.String `tfsdk:"from"`
	To   types.String `tfsdk:"to"`
}

type workflowAssignModel struct {
	Type      types.String `tfsdk:"type"`
	AccountID types.String `tfsdk:"account_id"`
}

var (
	workflowStatusAttrTypes     = map[string]attr.Type{"status_id": types.StringType}
	workflowSoDAttrTypes        = map[string]attr.Type{"from": types.StringType, "to": types.StringType}
	workflowAssignAttrTypes     = map[string]attr.Type{"type": types.StringType, "account_id": types.StringType}
	workflowTransitionAttrTypes = map[string]attr.Type{
		"name":                 types.StringType,
		"type":                 types.StringType,
		"from":                 types.ListType{ElemType: types.StringType},
		"to":                   types.StringType,
		"allowed_groups":       types.ListType{ElemType: types.StringType},
		"allowed_roles":        types.ListType{ElemType: types.StringType},
		"allowed_account_ids":  types.ListType{ElemType: types.StringType},
		"separation_of_duties": types.ListType{ElemType: types.ObjectType{AttrTypes: workflowSoDAttrTypes}},
		"assign":               types.ObjectType{AttrTypes: workflowAssignAttrTypes},
		"required_fields":      types.ListType{ElemType: types.StringType},
	}
)

func emptyStringList() types.List {
	return types.ListValueMust(types.StringType, []attr.Value{})
}

func (r *workflowResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_jira_workflow"
}

func (r *workflowResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	stringList := func(desc string) schema.ListAttribute {
		return schema.ListAttribute{
			Description: desc,
			Optional:    true,
			Computed:    true,
			ElementType: types.StringType,
			Default:     listdefault.StaticValue(emptyStringList()),
		}
	}

	resp.Schema = schema.Schema{
		Description: "Manages a company-managed (global) Jira Cloud workflow through the versioned workflow API: " +
			"its statuses, transitions, who may perform each transition (groups, roles, accounts), separation of duties, " +
			"the assignee set after a transition and required-field validators. The resource owns the whole workflow: " +
			"transitions not listed here are removed on apply; rules it does not manage on a listed transition are preserved. " +
			"An initial transition named `Create` to the first status is added automatically. Renaming a transition replaces it.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "The entity ID (UUID) of the workflow.",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Description: "The name of the workflow. Changing this forces recreation of the resource.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"description": schema.StringAttribute{
				Description: "The description of the workflow.",
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(""),
			},
			"version": schema.Int64Attribute{
				Description: "The document version number Jira assigns; used as an optimistic lock on update.",
				Computed:    true,
			},
			"statuses": schema.ListNestedAttribute{
				Description: "Existing global statuses used by the workflow, in layout order. The first one is the initial status.",
				Required:    true,
				Validators:  []validator.List{listvalidator.SizeAtLeast(1)},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"status_id": schema.StringAttribute{
							Description: "The ID of an existing global status (see `atlassian_jira_status`).",
							Required:    true,
						},
					},
				},
			},
			"transitions": schema.ListNestedAttribute{
				Description: "Transitions between statuses. Names must be unique within the workflow.",
				Required:    true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"name": schema.StringAttribute{
							Description: "The transition name shown to users. Unique within the workflow; `Create` is reserved.",
							Required:    true,
						},
						"type": schema.StringAttribute{
							Description: "`DIRECTED` (from specific statuses) or `GLOBAL` (from any status). Defaults to `DIRECTED`.",
							Optional:    true,
							Computed:    true,
							Default:     stringdefault.StaticString(transitionTypeDirected),
							Validators:  []validator.String{stringvalidator.OneOf(transitionTypeDirected, transitionTypeGlobal)},
						},
						"from": stringList("Status IDs the transition can start from. Required for DIRECTED, must be empty for GLOBAL."),
						"to": schema.StringAttribute{
							Description: "Status ID the transition leads to.",
							Required:    true,
						},
						"allowed_groups":      stringList("Group IDs whose members may perform the transition (system:restrict-issue-transition)."),
						"allowed_roles":       stringList("Project role IDs whose members may perform the transition."),
						"allowed_account_ids": stringList("Account IDs that may perform the transition."),
						"separation_of_duties": schema.ListNestedAttribute{
							Description: "Users who moved the issue from `from` to `to` may not perform this transition (system:separation-of-duties).",
							Optional:    true,
							Computed:    true,
							Default:     listdefault.StaticValue(types.ListValueMust(types.ObjectType{AttrTypes: workflowSoDAttrTypes}, []attr.Value{})),
							NestedObject: schema.NestedAttributeObject{
								Attributes: map[string]schema.Attribute{
									"from": schema.StringAttribute{Description: "Status ID the earlier move started from.", Required: true},
									"to":   schema.StringAttribute{Description: "Status ID the earlier move led to.", Required: true},
								},
							},
						},
						"assign": schema.SingleNestedAttribute{
							Description: "Assignee set after the transition (system:change-assignee post function).",
							Optional:    true,
							Attributes: map[string]schema.Attribute{
								"type": schema.StringAttribute{
									Description: "One of `to-selected-user`, `to-reporter`, `to-current-user`, `to-lead`, `to-unassigned`, `to-default-user`.",
									Required:    true,
								},
								"account_id": schema.StringAttribute{
									Description: "Account ID to assign; required when `type` is `to-selected-user`.",
									Optional:    true,
									Computed:    true,
									Default:     stringdefault.StaticString(""),
								},
							},
						},
						"required_fields": stringList("Field IDs that must be filled before the transition (one system:validate-field-value validator each)."),
					},
				},
			},
		},
	}
}

func (r *workflowResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// ---- model ⇄ spec -----------------------------------------------------------

func specFromModel(ctx context.Context, m workflowResourceModel) (workflowSpec, diag.Diagnostics) {
	var diags diag.Diagnostics
	spec := workflowSpec{Name: m.Name.ValueString(), Description: m.Description.ValueString()}

	var statuses []workflowStatusModel
	diags.Append(m.Statuses.ElementsAs(ctx, &statuses, false)...)
	for _, s := range statuses {
		spec.StatusIDs = append(spec.StatusIDs, s.StatusID.ValueString())
	}

	var transitions []workflowTransitionModel
	diags.Append(m.Transitions.ElementsAs(ctx, &transitions, false)...)
	for _, t := range transitions {
		ts := workflowTransitionSpec{Name: t.Name.ValueString(), Type: t.Type.ValueString(), To: t.To.ValueString()}
		diags.Append(t.From.ElementsAs(ctx, &ts.From, false)...)
		diags.Append(t.AllowedGroups.ElementsAs(ctx, &ts.AllowedGroups, false)...)
		diags.Append(t.AllowedRoles.ElementsAs(ctx, &ts.AllowedRoles, false)...)
		diags.Append(t.AllowedAccountIDs.ElementsAs(ctx, &ts.AllowedAccountIDs, false)...)
		diags.Append(t.RequiredFields.ElementsAs(ctx, &ts.RequiredFields, false)...)
		var sods []workflowSoDModel
		diags.Append(t.SeparationOfDuties.ElementsAs(ctx, &sods, false)...)
		for _, p := range sods {
			ts.SeparationOfDuties = append(ts.SeparationOfDuties, workflowSoDSpec{From: p.From.ValueString(), To: p.To.ValueString()})
		}
		if !t.Assign.IsNull() && !t.Assign.IsUnknown() {
			var a workflowAssignModel
			diags.Append(t.Assign.As(ctx, &a, basetypes.ObjectAsOptions{})...)
			ts.Assign = &workflowAssignSpec{Type: a.Type.ValueString(), AccountID: a.AccountID.ValueString()}
		}
		spec.Transitions = append(spec.Transitions, ts)
	}
	return spec, diags
}

func stringListValue(ctx context.Context, v []string) (types.List, diag.Diagnostics) {
	if v == nil {
		v = []string{}
	}
	return types.ListValueFrom(ctx, types.StringType, v)
}

func transitionsListValue(ctx context.Context, spec workflowSpec) (types.List, diag.Diagnostics) {
	var diags diag.Diagnostics
	elems := make([]attr.Value, 0, len(spec.Transitions))
	for _, ts := range spec.Transitions {
		from, d := stringListValue(ctx, ts.From)
		diags.Append(d...)
		groups, d := stringListValue(ctx, ts.AllowedGroups)
		diags.Append(d...)
		roles, d := stringListValue(ctx, ts.AllowedRoles)
		diags.Append(d...)
		accounts, d := stringListValue(ctx, ts.AllowedAccountIDs)
		diags.Append(d...)
		required, d := stringListValue(ctx, ts.RequiredFields)
		diags.Append(d...)

		sods := make([]attr.Value, 0, len(ts.SeparationOfDuties))
		for _, p := range ts.SeparationOfDuties {
			o, d := types.ObjectValue(workflowSoDAttrTypes, map[string]attr.Value{
				"from": types.StringValue(p.From), "to": types.StringValue(p.To),
			})
			diags.Append(d...)
			sods = append(sods, o)
		}
		sodList, d := types.ListValue(types.ObjectType{AttrTypes: workflowSoDAttrTypes}, sods)
		diags.Append(d...)

		assign := types.ObjectNull(workflowAssignAttrTypes)
		if ts.Assign != nil {
			assign, d = types.ObjectValue(workflowAssignAttrTypes, map[string]attr.Value{
				"type": types.StringValue(ts.Assign.Type), "account_id": types.StringValue(ts.Assign.AccountID),
			})
			diags.Append(d...)
		}

		obj, d := types.ObjectValue(workflowTransitionAttrTypes, map[string]attr.Value{
			"name":                 types.StringValue(ts.Name),
			"type":                 types.StringValue(ts.Type),
			"from":                 from,
			"to":                   types.StringValue(ts.To),
			"allowed_groups":       groups,
			"allowed_roles":        roles,
			"allowed_account_ids":  accounts,
			"separation_of_duties": sodList,
			"assign":               assign,
			"required_fields":      required,
		})
		diags.Append(d...)
		elems = append(elems, obj)
	}
	list, d := types.ListValue(types.ObjectType{AttrTypes: workflowTransitionAttrTypes}, elems)
	diags.Append(d...)
	return list, diags
}

func statusesListValue(ctx context.Context, ids []string) (types.List, diag.Diagnostics) {
	var diags diag.Diagnostics
	elems := make([]attr.Value, 0, len(ids))
	for _, id := range ids {
		o, d := types.ObjectValue(workflowStatusAttrTypes, map[string]attr.Value{"status_id": types.StringValue(id)})
		diags.Append(d...)
		elems = append(elems, o)
	}
	list, d := types.ListValue(types.ObjectType{AttrTypes: workflowStatusAttrTypes}, elems)
	diags.Append(d...)
	return list, diags
}

// modelFromDocument fills m from a server document (Read / import / data source).
func modelFromDocument(ctx context.Context, m *workflowResourceModel, doc jiraWorkflow, refToID map[string]string) diag.Diagnostics {
	var diags diag.Diagnostics
	spec, err := specFromDocument(doc, refToID)
	if err != nil {
		diags.AddError("Error reading workflow", err.Error())
		return diags
	}
	m.ID = types.StringValue(doc.ID)
	m.Name = types.StringValue(doc.Name)
	m.Description = types.StringValue(doc.Description)
	if doc.Version != nil {
		m.Version = types.Int64Value(int64(doc.Version.VersionNumber))
	}
	statuses, d := statusesListValue(ctx, spec.StatusIDs)
	diags.Append(d...)
	m.Statuses = statuses
	transitions, d := transitionsListValue(ctx, spec)
	diags.Append(d...)
	m.Transitions = transitions
	return diags
}

// ---- API helpers ----------------------------------------------------------------

// statusDefsFor looks up name/category for the given status ids (needed in
// the top-level statuses array of create/update requests). refs maps
// status id → reference already used by the server document, if any.
func (r *workflowResource) statusDefsFor(ctx context.Context, ids []string, refs map[string]string) (map[string]workflowStatusDef, error) {
	all, err := findAllStatuses(ctx, r.client)
	if err != nil {
		return nil, fmt.Errorf("listing statuses: %w", err)
	}
	byID := make(map[string]statusAPIItem, len(all))
	for _, s := range all {
		byID[s.ID] = s
	}
	defs := make(map[string]workflowStatusDef, len(ids))
	var missing []string
	for _, id := range ids {
		s, ok := byID[id]
		if !ok {
			missing = append(missing, id)
			continue
		}
		defs[id] = workflowStatusDef{ID: id, StatusReference: refs[id], Name: s.Name, StatusCategory: s.statusCategoryKey()}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("statuses not found: %s", strings.Join(missing, ", "))
	}
	return defs, nil
}

// fetchWorkflow bulk-gets one workflow. found=false on an empty result.
func fetchWorkflow(ctx context.Context, client *atlassian.Client, req workflowReadRequest) (jiraWorkflow, map[string]string, bool, error) {
	var resp workflowReadResponse
	if err := client.Post(ctx, "/rest/api/3/workflows", req, &resp); err != nil {
		return jiraWorkflow{}, nil, false, err
	}
	if len(resp.Workflows) == 0 {
		return jiraWorkflow{}, nil, false, nil
	}
	refToID, err := statusRefIndex(resp.Statuses)
	if err != nil {
		return jiraWorkflow{}, nil, false, err
	}
	return resp.Workflows[0], refToID, true, nil
}

// validate calls the create/update validation endpoint and turns its ERROR
// entries into one readable error.
func (r *workflowResource) validate(ctx context.Context, kind string, payload interface{}) error {
	var resp workflowValidationResponse
	req := workflowValidationRequest{Payload: payload, ValidationOptions: workflowValidationOptions{Levels: []string{"ERROR"}}}
	if err := r.client.Post(ctx, "/rest/api/3/workflows/"+kind+"/validation", req, &resp); err != nil {
		return err
	}
	var msgs []string
	for _, e := range resp.Errors {
		if e.Level != "" && e.Level != "ERROR" {
			continue
		}
		msgs = append(msgs, fmt.Sprintf("%s: %s", e.Code, e.Message))
	}
	if len(msgs) > 0 {
		return fmt.Errorf("workflow rejected by Jira:\n  - %s", strings.Join(msgs, "\n  - "))
	}
	return nil
}

// postWithConflictRetry posts and retries once after a 409.
func (r *workflowResource) postWithConflictRetry(ctx context.Context, apiPath string, body interface{}, out interface{}) error {
	status, err := r.client.PostWithStatus(ctx, apiPath, body, out)
	if status != http.StatusConflict {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(workflowConflictRetryDelay):
	}
	_, err = r.client.PostWithStatus(ctx, apiPath, body, out)
	return err
}

// ---- CRUD ------------------------------------------------------------------------

func (r *workflowResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan workflowResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	spec, diags := specFromModel(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := validateSpec(spec); err != nil {
		resp.Diagnostics.AddError("Invalid workflow configuration", err.Error())
		return
	}

	defs, err := r.statusDefsFor(ctx, spec.StatusIDs, nil)
	if err != nil {
		resp.Diagnostics.AddError("Error creating workflow", err.Error())
		return
	}
	body, err := buildCreateRequest(spec, defs)
	if err != nil {
		resp.Diagnostics.AddError("Invalid workflow configuration", err.Error())
		return
	}
	if err := r.validate(ctx, "create", body); err != nil {
		resp.Diagnostics.AddError("Error creating workflow", err.Error())
		return
	}

	var result workflowReadResponse
	if err := r.postWithConflictRetry(ctx, "/rest/api/3/workflows/create", body, &result); err != nil {
		resp.Diagnostics.AddError("Error creating workflow", err.Error())
		return
	}
	if len(result.Workflows) == 0 {
		resp.Diagnostics.AddError("Error creating workflow", "API returned no workflow")
		return
	}
	created := result.Workflows[0]

	// Preserve plan values; take id and version from the server.
	plan.ID = types.StringValue(created.ID)
	plan.Version = types.Int64Value(1)
	if created.Version != nil {
		plan.Version = types.Int64Value(int64(created.Version.VersionNumber))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *workflowResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state workflowResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	doc, refToID, found, err := fetchWorkflow(ctx, r.client, workflowReadRequest{WorkflowIDs: []string{state.ID.ValueString()}})
	if err != nil {
		resp.Diagnostics.AddError("Error reading workflow", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}

	resp.Diagnostics.Append(modelFromDocument(ctx, &state, doc, refToID)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *workflowResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state workflowResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	spec, diags := specFromModel(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := validateSpec(spec); err != nil {
		resp.Diagnostics.AddError("Invalid workflow configuration", err.Error())
		return
	}

	// Always merge into the freshest server document (version lock, rule ids).
	id := state.ID.ValueString()
	current, refToID, found, err := fetchWorkflow(ctx, r.client, workflowReadRequest{WorkflowIDs: []string{id}})
	if err != nil {
		resp.Diagnostics.AddError("Error updating workflow", err.Error())
		return
	}
	if !found {
		resp.Diagnostics.AddError("Error updating workflow", fmt.Sprintf("workflow %s no longer exists", id))
		return
	}
	idToRef := make(map[string]string, len(refToID))
	for ref, sid := range refToID {
		idToRef[sid] = ref
	}
	defs, err := r.statusDefsFor(ctx, spec.StatusIDs, idToRef)
	if err != nil {
		resp.Diagnostics.AddError("Error updating workflow", err.Error())
		return
	}
	item, err := buildUpdateItem(spec, current, defs)
	if err != nil {
		resp.Diagnostics.AddError("Invalid workflow configuration", err.Error())
		return
	}
	topLevel := make([]workflowStatusDef, 0, len(spec.StatusIDs))
	for _, sid := range spec.StatusIDs {
		d := defs[sid]
		topLevel = append(topLevel, workflowStatusDef{ID: sid, StatusReference: refOf(defs, sid), Name: d.Name, StatusCategory: d.StatusCategory})
	}
	body := workflowUpdateRequest{Statuses: topLevel, Workflows: []workflowUpdateItem{item}}

	if err := r.validate(ctx, "update", body); err != nil {
		resp.Diagnostics.AddError("Error updating workflow", err.Error())
		return
	}

	var result workflowReadResponse
	if err := r.postWithConflictRetry(ctx, "/rest/api/3/workflows/update", body, &result); err != nil {
		resp.Diagnostics.AddError("Error updating workflow", err.Error())
		return
	}
	if result.TaskID != "" {
		if err := r.client.PollTask(ctx, result.TaskID); err != nil {
			resp.Diagnostics.AddError("Error updating workflow", err.Error())
			return
		}
	}

	plan.ID = state.ID
	switch {
	case len(result.Workflows) > 0 && result.Workflows[0].Version != nil:
		plan.Version = types.Int64Value(int64(result.Workflows[0].Version.VersionNumber))
	default:
		// Asynchronous update: read the new version back.
		doc, _, found, err := fetchWorkflow(ctx, r.client, workflowReadRequest{WorkflowIDs: []string{id}})
		if err == nil && found && doc.Version != nil {
			plan.Version = types.Int64Value(int64(doc.Version.VersionNumber))
		} else {
			plan.Version = types.Int64Value(state.Version.ValueInt64() + 1)
		}
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *workflowResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state workflowResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiPath := "/rest/api/3/workflow/" + atlassian.PathEscape(state.ID.ValueString())
	statusCode, err := r.client.DeleteWithStatus(ctx, apiPath)

	// 404 means the workflow was already deleted out-of-band; treat as success.
	if statusCode == http.StatusNotFound {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Error deleting workflow",
			err.Error()+"\n\nA workflow that is active or still referenced by a workflow scheme cannot be deleted; remove the scheme association first.")
		return
	}
}

func (r *workflowResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

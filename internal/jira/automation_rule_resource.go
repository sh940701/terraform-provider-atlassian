package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

var (
	_ resource.Resource                = &automationRuleResource{}
	_ resource.ResourceWithImportState = &automationRuleResource{}
)

// NewAutomationRuleResource returns a new Jira automation rule resource.
func NewAutomationRuleResource() resource.Resource {
	return &automationRuleResource{}
}

type automationRuleResource struct {
	client *atlassian.Client
}

type automationRuleResourceModel struct {
	ID                  types.String  `tfsdk:"id"`
	UUID                types.String  `tfsdk:"uuid"`
	Name                types.String  `tfsdk:"name"`
	Description         types.String  `tfsdk:"description"`
	State               types.String  `tfsdk:"state"`
	ProjectIDs          types.Set     `tfsdk:"project_ids"`
	ExtraScopeARIs      types.List    `tfsdk:"extra_scope_aris"`
	Body                RuleBodyValue `tfsdk:"body"`
	ActorAccountID      types.String  `tfsdk:"actor_account_id"`
	CanOtherRuleTrigger types.Bool    `tfsdk:"can_other_rule_trigger"`
	NotifyOnError       types.String  `tfsdk:"notify_on_error"`
}

const (
	ruleStateEnabled  = "ENABLED"
	ruleStateDisabled = "DISABLED"

	// ruleNotifyOnErrorDefault is the schema default for notify_on_error.
	// The set of valid values is unverified against a real site (T5/T6),
	// so this is kept as a free-form string rather than a validated enum.
	ruleNotifyOnErrorDefault = "FIRSTERROR"
)

func (r *automationRuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_jira_automation_rule"
}

func (r *automationRuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a Jira Cloud automation rule via the Automation Rule Management API " +
			"(https://api.atlassian.com/automation/public/jira). `body` carries the rule's trigger and " +
			"components as an opaque JSON object — this resource does not model automation's component graph, " +
			"only the rule's identity, scope, and enabled state. Import by the rule's `uuid`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "The rule's UUID (same value as `uuid`).",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"uuid": schema.StringAttribute{
				Description: "The rule's UUID, assigned by Jira on creation.",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Description: "The rule's name.",
				Required:    true,
			},
			"description": schema.StringAttribute{
				Description: "The rule's description.",
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(""),
			},
			"state": schema.StringAttribute{
				Description: "Whether the rule is enabled. One of `ENABLED`, `DISABLED`. Defaults to `ENABLED`.",
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(ruleStateEnabled),
				Validators: []validator.String{
					stringvalidator.OneOf(ruleStateEnabled, ruleStateDisabled),
				},
			},
			"project_ids": schema.SetAttribute{
				Description: "Project ids the rule is scoped to. Translated to `ruleScopeARIs` " +
					"(`ari:cloud:jira:{cloudId}:project/{projectId}`) on write. Order does not matter.",
				Required:    true,
				ElementType: types.StringType,
			},
			"extra_scope_aris": schema.ListAttribute{
				Description: "Non-project scope ARIs the server has recorded for this rule (e.g. a board " +
					"or filter scope) that `project_ids` does not model — read-only, and carried through " +
					"unchanged whenever `project_ids` is updated.",
				Computed:    true,
				ElementType: types.StringType,
				PlanModifiers: []planmodifier.List{
					listplanmodifier.UseStateForUnknown(),
				},
			},
			"body": schema.StringAttribute{
				Description: `The rule's trigger and components as a JSON object: {"trigger": {...}, "components": [...]}. ` +
					"Opaque to this resource — whitespace, key-order, and server-added `id`/`schemaVersion`/empty " +
					"`conditions`/`children` differences are not drift.",
				Required:   true,
				CustomType: RuleBodyType{},
			},
			"actor_account_id": schema.StringAttribute{
				Description: "Account id the rule's actions run as. Left unset, Jira assigns its own " +
					"default (typically the rule's author).",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.String{
					// Without this, an update to any other attribute would
					// replan this one as unknown (no Default to fall back
					// to) even though nothing about it changed.
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"can_other_rule_trigger": schema.BoolAttribute{
				Description: "Whether this rule's actions are allowed to trigger other automation rules. " +
					"Defaults to `false`.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
			},
			"notify_on_error": schema.StringAttribute{
				Description: "When to notify the rule's actor on error. The set of valid values is " +
					"unverified against a real site, so this is a free-form string rather than a validated " +
					"enum. Defaults to `FIRSTERROR`.",
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString(ruleNotifyOnErrorDefault),
			},
		},
	}
}

func (r *automationRuleResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// createRuleRequest is the POST /rule request body.
type createRuleRequest struct {
	Rule ruleDoc `json:"rule"`
}

// createRuleResponse is the POST /rule response. Its exact shape is
// unconfirmed (T5): GET returns the rule document at the top level, but the
// create response might either echo that shape directly (ruleDoc embedded
// here promotes its fields, including "uuid", to the top level) or wrap it
// under a "rule" key — so both are checked for uuid.
type createRuleResponse struct {
	ruleDoc
	Rule *ruleDoc `json:"rule,omitempty"`
}

// updateRuleRequest is the PUT /rule/{uuid} request body — mirrors
// createRuleRequest's {"rule": ruleDoc} wrapping since Update and Create
// otherwise share the same document shape (see ruleDoc's doc comment: this
// wrapping is unconfirmed against a real site for Update specifically).
type updateRuleRequest struct {
	Rule ruleDoc `json:"rule"`
}

// ruleScopeRequest is the PUT /rule/{uuid}/rule-scope request body. The
// exact payload key ("ruleScopeARIs") is unverified against a real site
// (T6) — isolated here, and behind putRuleScope, so a follow-up task can
// adjust it in one place.
type ruleScopeRequest struct {
	RuleScopeARIs []string `json:"ruleScopeARIs"`
}

// ruleStateRequest is the PUT /rule/{uuid}/state request body.
type ruleStateRequest struct {
	State string `json:"state"`
}

// putRuleScope calls PUT /rule/{uuid}/rule-scope with the full scope ARI
// set (project ARIs + extra/opaque ARIs), returning the response status
// code so callers can treat a 404 as "already gone" like elsewhere in this
// resource.
func (r *automationRuleResource) putRuleScope(ctx context.Context, uuid string, aris []string) (int, error) {
	scopePath, err := r.client.AutomationURL(ctx, "/rule/"+atlassian.PathEscape(uuid)+"/rule-scope")
	if err != nil {
		return 0, fmt.Errorf("building Automation API URL: %w", err)
	}
	return r.client.PutWithStatus(ctx, scopePath, ruleScopeRequest{RuleScopeARIs: aris}, nil)
}

// putRuleState calls PUT /rule/{uuid}/state with the desired ENABLED/DISABLED
// state, returning the response status code (see putRuleScope). Package-level
// so the sweeper can reuse the same payload shape as the resource.
func putRuleState(ctx context.Context, client *atlassian.Client, uuid, state string) (int, error) {
	statePath, err := client.AutomationURL(ctx, "/rule/"+atlassian.PathEscape(uuid)+"/state")
	if err != nil {
		return 0, fmt.Errorf("building Automation API URL: %w", err)
	}
	return client.PutWithStatus(ctx, statePath, ruleStateRequest{State: state}, nil)
}

func (r *automationRuleResource) putRuleState(ctx context.Context, uuid, state string) (int, error) {
	return putRuleState(ctx, r.client, uuid, state)
}

// RuleSummary is one row of GET /rule/summary (S1 spike: {uuid, name, state, ...}).
type RuleSummary struct {
	UUID  string `json:"uuid"`
	Name  string `json:"name"`
	State string `json:"state"`
}

// ListAutomationRuleSummaries pages GET /rule/summary.
// Envelope is {"data": [...], "links": {"next": ...}} — observed on a real
// site (S1). A 200 whose envelope has no "data" key is an error, not an
// empty list: that used to let a guessed GET /rules shape silently skip
// leftover tf-acc-test-* rules.
func ListAutomationRuleSummaries(ctx context.Context, client *atlassian.Client) ([]RuleSummary, error) {
	path, err := client.AutomationURL(ctx, "/rule/summary")
	if err != nil {
		return nil, fmt.Errorf("building automation rule summary URL: %w", err)
	}

	var all []RuleSummary
	for page := 0; page < atlassian.MaxPages; page++ {
		var envelope map[string]json.RawMessage
		if err := client.Get(ctx, path, &envelope); err != nil {
			return nil, fmt.Errorf("listing automation rule summaries: %w", err)
		}
		data, ok := envelope["data"]
		if !ok {
			return nil, fmt.Errorf("GET /rule/summary: response missing data key")
		}
		var summaries []RuleSummary
		if err := json.Unmarshal(data, &summaries); err != nil {
			return nil, fmt.Errorf("GET /rule/summary: decoding data: %w", err)
		}
		all = append(all, summaries...)

		var links struct {
			Next string `json:"next"`
		}
		if raw, ok := envelope["links"]; ok && len(raw) > 0 {
			if err := json.Unmarshal(raw, &links); err != nil {
				return nil, fmt.Errorf("GET /rule/summary: decoding links: %w", err)
			}
		}
		if links.Next == "" {
			return all, nil
		}
		// Next is an absolute Automation API URL on a real site; Client.Get
		// accepts it because the origin is already on the allowlist.
		path = links.Next
	}
	return nil, fmt.Errorf("listing automation rule summaries: exceeded %d pages", atlassian.MaxPages)
}

// DeleteAutomationRule disables then deletes, matching the resource Delete
// sequence. A 404 on either step is treated as already gone.
func DeleteAutomationRule(ctx context.Context, client *atlassian.Client, uuid string) error {
	disableStatus, err := putRuleState(ctx, client, uuid, ruleStateDisabled)
	if err != nil && disableStatus != http.StatusNotFound {
		return fmt.Errorf("disabling before delete: %w", err)
	}

	rulePath, err := client.AutomationURL(ctx, "/rule/"+atlassian.PathEscape(uuid))
	if err != nil {
		return fmt.Errorf("building Automation API URL: %w", err)
	}

	statusCode, err := client.DeleteWithStatus(ctx, rulePath)
	if statusCode == http.StatusNotFound {
		return nil
	}
	return err
}

func (r *automationRuleResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan automationRuleResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var projectIDs []string
	resp.Diagnostics.Append(plan.ProjectIDs.ElementsAs(ctx, &projectIDs, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	cloudID, err := r.client.CloudID(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Error creating automation rule", fmt.Sprintf("looking up cloud ID: %s", err))
		return
	}

	// A brand new rule has no server-recorded non-project scopes yet.
	doc, err := docFromRule(cloudID, plan.Name.ValueString(), plan.Description.ValueString(), plan.State.ValueString(),
		projectIDs, nil, plan.Body.ValueString(), plan.ActorAccountID.ValueString(),
		plan.CanOtherRuleTrigger.ValueBool(), plan.NotifyOnError.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error creating automation rule", err.Error())
		return
	}

	rulePath, err := r.client.AutomationURL(ctx, "/rule")
	if err != nil {
		resp.Diagnostics.AddError("Error creating automation rule", fmt.Sprintf("building Automation API URL: %s", err))
		return
	}

	var result createRuleResponse
	if err := r.client.Post(ctx, rulePath, createRuleRequest{Rule: doc}, &result); err != nil {
		resp.Diagnostics.AddError("Error creating automation rule", err.Error())
		return
	}

	uuid := result.UUID
	if uuid == "" && result.Rule != nil {
		uuid = result.Rule.UUID
	}
	if uuid == "" {
		resp.Diagnostics.AddError("Error creating automation rule", "create response carried no rule uuid")
		return
	}

	// Server-generated: uuid/id. Everything else is preserved from the plan
	// (user intent) rather than re-read from the response, whose shape is
	// unconfirmed beyond uuid — except ruleScopeARIs, checked below in both
	// possible response shapes, since Jira may add a scope of its own on
	// create (e.g. one auto-derived from the actor) that this resource
	// would otherwise never learn about until the next Read.
	plan.UUID = types.StringValue(uuid)
	plan.ID = types.StringValue(uuid)
	if plan.ActorAccountID.IsNull() || plan.ActorAccountID.IsUnknown() {
		// Nothing was configured; record what was actually sent (nothing —
		// actorFromAccountID("") omits the field) so Read's own "" default
		// matches and no drift shows up. See ruleActor's doc comment.
		plan.ActorAccountID = types.StringValue("")
	}

	responseScopeARIs := result.RuleScopeARIs
	if len(responseScopeARIs) == 0 && result.Rule != nil {
		responseScopeARIs = result.Rule.RuleScopeARIs
	}
	if len(responseScopeARIs) == 0 {
		// The response carried no scope info either way — fall back to what
		// was actually sent, which has no extras (a brand new rule can only
		// have been given project scopes at this point).
		responseScopeARIs = doc.RuleScopeARIs
	}

	extraScopeARIs, diags := types.ListValueFrom(ctx, types.StringType, extraScopeARIsFromARIs(responseScopeARIs))
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ExtraScopeARIs = extraScopeARIs

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *automationRuleResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state automationRuleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	rulePath, err := r.client.AutomationURL(ctx, "/rule/"+atlassian.PathEscape(state.UUID.ValueString()))
	if err != nil {
		resp.Diagnostics.AddError("Error reading automation rule", fmt.Sprintf("building Automation API URL: %s", err))
		return
	}

	var doc ruleDoc
	statusCode, err := r.client.GetWithStatus(ctx, rulePath, &doc)
	if err != nil {
		resp.Diagnostics.AddError("Error reading automation rule", err.Error())
		return
	}
	if statusCode == http.StatusNotFound {
		resp.State.RemoveResource(ctx)
		return
	}

	newBody, err := bodyFromDoc(doc)
	if err != nil {
		resp.Diagnostics.AddError("Error reading automation rule", err.Error())
		return
	}

	oldBody := ""
	if !state.Body.IsNull() && !state.Body.IsUnknown() {
		oldBody = state.Body.ValueString()
	}
	bodyToStore, err := bodyForState(oldBody, newBody)
	if err != nil {
		resp.Diagnostics.AddError("Error reading automation rule", fmt.Sprintf("comparing rule body: %s", err))
		return
	}

	projectIDs, diags := types.SetValueFrom(ctx, types.StringType, projectIDsFromARIs(doc.RuleScopeARIs))
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	extraScopeARIs, diags := types.ListValueFrom(ctx, types.StringType, extraScopeARIsFromARIs(doc.RuleScopeARIs))
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	state.Name = types.StringValue(doc.Name)
	state.Description = types.StringValue(doc.Description)
	state.State = types.StringValue(doc.State)
	state.ProjectIDs = projectIDs
	state.ExtraScopeARIs = extraScopeARIs
	state.Body = NewRuleBodyValue(bodyToStore)
	state.ActorAccountID = types.StringValue(doc.Actor.accountID())
	state.CanOtherRuleTrigger = types.BoolValue(doc.CanOtherRuleTrigger)
	state.NotifyOnError = types.StringValue(doc.NotifyOnError)
	// doc.UUID is expected to echo the id we just requested by, but if a
	// response ever omits it, keep the prior state's uuid/id rather than
	// wiping the resource's identifier — losing it would make every
	// subsequent Read/Update/Delete address /rule/ (no id) instead of
	// disappearing cleanly or erroring loudly here.
	if doc.UUID != "" {
		state.UUID = types.StringValue(doc.UUID)
		state.ID = types.StringValue(doc.UUID)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update always sends the full rule document to PUT /rule/{uuid} (per
// ruleDoc/updateRuleRequest — no attribute in this schema has
// RequiresReplace, so any change to name/description/state/project_ids/
// body/actor_account_id/can_other_rule_trigger/notify_on_error lands here),
// then additionally calls the dedicated rule-scope endpoint when
// project_ids changed and the dedicated state endpoint when state changed —
// per the Automation Rule Management API's split update surface (T6).
func (r *automationRuleResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state automationRuleResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var projectIDs []string
	resp.Diagnostics.Append(plan.ProjectIDs.ElementsAs(ctx, &projectIDs, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// extra_scope_aris is Computed+UseStateForUnknown (this resource never
	// asks the user to write it), so the prior state's value is what a plain
	// attribute update carries forward untouched.
	var extraScopeARIs []string
	resp.Diagnostics.Append(state.ExtraScopeARIs.ElementsAs(ctx, &extraScopeARIs, false)...)
	if resp.Diagnostics.HasError() {
		return
	}

	cloudID, err := r.client.CloudID(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Error updating automation rule", fmt.Sprintf("looking up cloud ID: %s", err))
		return
	}

	uuid := state.UUID.ValueString()

	doc, err := docFromRule(cloudID, plan.Name.ValueString(), plan.Description.ValueString(), plan.State.ValueString(),
		projectIDs, extraScopeARIs, plan.Body.ValueString(), plan.ActorAccountID.ValueString(),
		plan.CanOtherRuleTrigger.ValueBool(), plan.NotifyOnError.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error updating automation rule", err.Error())
		return
	}
	doc.UUID = uuid

	rulePath, err := r.client.AutomationURL(ctx, "/rule/"+atlassian.PathEscape(uuid))
	if err != nil {
		resp.Diagnostics.AddError("Error updating automation rule", fmt.Sprintf("building Automation API URL: %s", err))
		return
	}

	if err := r.client.Put(ctx, rulePath, updateRuleRequest{Rule: doc}, nil); err != nil {
		resp.Diagnostics.AddError("Error updating automation rule", err.Error())
		return
	}

	// project_ids' Set semantics (order-insensitive Equal) mean this only
	// fires on an actual scope change, never a reordering.
	if !plan.ProjectIDs.Equal(state.ProjectIDs) {
		if _, err := r.putRuleScope(ctx, uuid, doc.RuleScopeARIs); err != nil {
			resp.Diagnostics.AddError("Error updating automation rule scope", err.Error())
			return
		}
	}

	if plan.State.ValueString() != state.State.ValueString() {
		if _, err := r.putRuleState(ctx, uuid, plan.State.ValueString()); err != nil {
			resp.Diagnostics.AddError("Error updating automation rule state", err.Error())
			return
		}
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete disables the rule before deleting it — the Automation Rule
// Management API only allows deleting a disabled rule — then deletes it.
// A 404 on either step is treated as the rule already being gone rather
// than an error.
func (r *automationRuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state automationRuleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	uuid := state.UUID.ValueString()

	if err := DeleteAutomationRule(ctx, r.client, uuid); err != nil {
		resp.Diagnostics.AddError("Error deleting automation rule", err.Error())
		return
	}
}

// ImportState imports a rule by its uuid: `terraform import ... <uuid>`.
// Both id and uuid are seeded from the import id (they're always equal —
// see Create), and Read fills in everything else.
func (r *automationRuleResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("uuid"), req.ID)...)
}

package jira

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-jsontypes/jsontypes"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

var _ resource.Resource = &automationRuleResource{}

// NewAutomationRuleResource returns a new Jira automation rule resource.
func NewAutomationRuleResource() resource.Resource {
	return &automationRuleResource{}
}

type automationRuleResource struct {
	client *atlassian.Client
}

type automationRuleResourceModel struct {
	ID             types.String         `tfsdk:"id"`
	UUID           types.String         `tfsdk:"uuid"`
	Name           types.String         `tfsdk:"name"`
	Description    types.String         `tfsdk:"description"`
	State          types.String         `tfsdk:"state"`
	ProjectIDs     types.List           `tfsdk:"project_ids"`
	Body           jsontypes.Normalized `tfsdk:"body"`
	ActorAccountID types.String         `tfsdk:"actor_account_id"`
}

const (
	ruleStateEnabled  = "ENABLED"
	ruleStateDisabled = "DISABLED"
)

func (r *automationRuleResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_jira_automation_rule"
}

func (r *automationRuleResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a Jira Cloud automation rule via the Automation Rule Management API " +
			"(https://api.atlassian.com/automation/public/jira). `body` carries the rule's trigger and " +
			"components as an opaque JSON object — this resource does not model automation's component graph, " +
			"only the rule's identity, scope, and enabled state. Update and import are not yet supported.",
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
				Description: "Whether the rule is enabled. One of `ENABLED`, `DISABLED`.",
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(ruleStateEnabled),
				Validators: []validator.String{
					stringvalidator.OneOf(ruleStateEnabled, ruleStateDisabled),
				},
			},
			"project_ids": schema.ListAttribute{
				Description: "Project ids the rule is scoped to. Translated to `ruleScopeARIs` " +
					"(`ari:cloud:jira:{cloudId}:project/{projectId}`) on write.",
				Required:    true,
				ElementType: types.StringType,
			},
			"body": schema.StringAttribute{
				Description: `The rule's trigger and components as a JSON object: {"trigger": {...}, "components": [...]}. ` +
					"Opaque to this resource — whitespace and key-order differences are not drift.",
				Required:   true,
				CustomType: jsontypes.NormalizedType{},
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

	doc, err := docFromRule(cloudID, plan.Name.ValueString(), plan.Description.ValueString(), plan.State.ValueString(),
		projectIDs, plan.Body.ValueString(), plan.ActorAccountID.ValueString())
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
	// unconfirmed beyond uuid.
	plan.UUID = types.StringValue(uuid)
	plan.ID = types.StringValue(uuid)
	if plan.ActorAccountID.IsNull() || plan.ActorAccountID.IsUnknown() {
		// Nothing was configured; record what was actually sent (nothing —
		// actorFromAccountID("") omits the field) so Read's own "" default
		// matches and no drift shows up. See ruleActor's doc comment.
		plan.ActorAccountID = types.StringValue("")
	}

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

	body, err := bodyFromDoc(doc)
	if err != nil {
		resp.Diagnostics.AddError("Error reading automation rule", err.Error())
		return
	}

	projectIDs, diags := types.ListValueFrom(ctx, types.StringType, projectIDsFromARIs(doc.RuleScopeARIs))
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	state.Name = types.StringValue(doc.Name)
	state.Description = types.StringValue(doc.Description)
	state.State = types.StringValue(doc.State)
	state.ProjectIDs = projectIDs
	state.Body = jsontypes.NewNormalizedValue(body)
	state.ActorAccountID = types.StringValue(doc.Actor.accountID())
	state.UUID = types.StringValue(doc.UUID)
	state.ID = types.StringValue(doc.UUID)

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is not implemented yet (T6): the Automation Rule Management API's
// update semantics are unconfirmed, and no attribute in this resource's
// schema currently carries a plan modifier that would route a change here
// without also going through Create — this method exists only so
// automationRuleResource satisfies resource.Resource.
func (r *automationRuleResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError(
		"Update not yet supported",
		"atlassian_jira_automation_rule does not support in-place updates yet. Destroy and recreate the rule "+
			"to change it, or wait for update support to land in a follow-up release.",
	)
}

func (r *automationRuleResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state automationRuleResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	rulePath, err := r.client.AutomationURL(ctx, "/rule/"+atlassian.PathEscape(state.UUID.ValueString()))
	if err != nil {
		resp.Diagnostics.AddError("Error deleting automation rule", fmt.Sprintf("building Automation API URL: %s", err))
		return
	}

	statusCode, err := r.client.DeleteWithStatus(ctx, rulePath)
	if statusCode == http.StatusNotFound {
		return // already gone
	}
	if err != nil {
		resp.Diagnostics.AddError("Error deleting automation rule", err.Error())
		return
	}
}

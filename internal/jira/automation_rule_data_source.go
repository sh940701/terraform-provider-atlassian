package jira

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

var _ datasource.DataSource = &automationRuleDataSource{}

// NewAutomationRuleDataSource returns a new automation rule data source.
func NewAutomationRuleDataSource() datasource.DataSource {
	return &automationRuleDataSource{}
}

type automationRuleDataSource struct {
	client *atlassian.Client
}

type automationRuleDataSourceModel struct {
	UUID                types.String `tfsdk:"uuid"`
	ID                  types.String `tfsdk:"id"`
	Name                types.String `tfsdk:"name"`
	Description         types.String `tfsdk:"description"`
	State               types.String `tfsdk:"state"`
	ProjectIDs          types.Set    `tfsdk:"project_ids"`
	ExtraScopeARIs      types.List   `tfsdk:"extra_scope_aris"`
	Body                types.String `tfsdk:"body"`
	ActorAccountID      types.String `tfsdk:"actor_account_id"`
	CanOtherRuleTrigger types.Bool   `tfsdk:"can_other_rule_trigger"`
	NotifyOnError       types.String `tfsdk:"notify_on_error"`
}

func (d *automationRuleDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_jira_automation_rule"
}

func (d *automationRuleDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Use this data source to look up a Jira Cloud automation rule by its UUID. Useful for debugging " +
			"or verifying a rule's configuration that may have been created outside of Terraform.",
		Attributes: map[string]schema.Attribute{
			"uuid": schema.StringAttribute{
				Description: "The automation rule's UUID.",
				Required:    true,
			},
			"id": schema.StringAttribute{
				Description: "The rule's UUID (same value as `uuid`).",
				Computed:    true,
			},
			"name": schema.StringAttribute{
				Description: "The rule's name.",
				Computed:    true,
			},
			"description": schema.StringAttribute{
				Description: "The rule's description.",
				Computed:    true,
			},
			"state": schema.StringAttribute{
				Description: "Whether the rule is enabled. One of `ENABLED`, `DISABLED`.",
				Computed:    true,
			},
			"project_ids": schema.SetAttribute{
				Description: "Project ids the rule is scoped to, extracted from project-scope ARIs " +
					"(`ari:cloud:jira:{cloudId}:project/{projectId}`).",
				Computed:    true,
				ElementType: types.StringType,
			},
			"extra_scope_aris": schema.ListAttribute{
				Description: "Non-project scope ARIs the server has recorded for this rule (e.g. a board " +
					"or filter scope) that `project_ids` does not model.",
				Computed:    true,
				ElementType: types.StringType,
			},
			"body": schema.StringAttribute{
				Description: `The rule's trigger and components as a JSON object: {"trigger": {...}, "components": [...]}. ` +
					"The server's serialization with sorted object keys, returned as a plain string. " +
					"This data source does not apply semantic equality (unlike the resource's body attribute), " +
					"so whitespace and key order changes are treated as modifications.",
				Computed: true,
			},
			"actor_account_id": schema.StringAttribute{
				Description: "Account id the rule's actions run as. Empty if the rule runs as Jira's default.",
				Computed:    true,
			},
			"can_other_rule_trigger": schema.BoolAttribute{
				Description: "Whether this rule's actions are allowed to trigger other automation rules.",
				Computed:    true,
			},
			"notify_on_error": schema.StringAttribute{
				Description: "When to notify the rule's actor on error.",
				Computed:    true,
			},
		},
	}
}

func (d *automationRuleDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	client, ok := req.ProviderData.(*atlassian.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected Data Source Configure Type",
			fmt.Sprintf("Expected *atlassian.Client, got: %T", req.ProviderData),
		)
		return
	}

	d.client = client
}

func (d *automationRuleDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config automationRuleDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	uuid := config.UUID.ValueString()

	// Get the rule from the Automation API
	rulePath, err := d.client.AutomationURL(ctx, "/rule/"+atlassian.PathEscape(uuid))
	if err != nil {
		resp.Diagnostics.AddError("Error building automation rule URL", err.Error())
		return
	}

	var apiResp ruleDoc
	statusCode, err := d.client.GetWithStatus(ctx, rulePath, &apiResp)
	if err != nil {
		resp.Diagnostics.AddError("Error reading automation rule", err.Error())
		return
	}

	if statusCode == 404 {
		resp.Diagnostics.AddError(
			"Automation rule not found",
			fmt.Sprintf("No automation rule found with UUID %q", uuid),
		)
		return
	}
	// GetWithStatus only returns (code, nil) for 200 and 404; any other
	// status is already in err above (and includes the response body).

	// Extract project IDs from the rule's scope ARIs
	projectIDs := projectIDsFromARIs(apiResp.RuleScopeARIs)
	extraScopeARIs := extraScopeARIsFromARIs(apiResp.RuleScopeARIs)

	// Re-serialize the trigger and components as the body JSON
	bodyStr, err := bodyFromDoc(apiResp)
	if err != nil {
		resp.Diagnostics.AddError("Error serializing rule body", err.Error())
		return
	}

	// Build the terraform model
	config.ID = types.StringValue(uuid)
	config.UUID = types.StringValue(uuid)
	config.Name = types.StringValue(apiResp.Name)
	config.Description = types.StringValue(apiResp.Description)
	config.State = types.StringValue(apiResp.State)
	config.ActorAccountID = types.StringValue(apiResp.Actor.accountID())
	config.CanOtherRuleTrigger = types.BoolValue(apiResp.CanOtherRuleTrigger)
	config.NotifyOnError = types.StringValue(apiResp.NotifyOnError)
	config.Body = types.StringValue(bodyStr)

	// Set project_ids as a set
	projectIDsSet, diags := types.SetValueFrom(ctx, types.StringType, projectIDs)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	config.ProjectIDs = projectIDsSet

	// Set extra_scope_aris as a list
	extraScopeARIsList, diags := types.ListValueFrom(ctx, types.StringType, extraScopeARIs)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	config.ExtraScopeARIs = extraScopeARIsList

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}

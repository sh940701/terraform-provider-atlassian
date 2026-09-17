package jira

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

var _ datasource.DataSource = &workflowDataSource{}

// NewWorkflowDataSource returns a new workflow data source.
func NewWorkflowDataSource() datasource.DataSource {
	return &workflowDataSource{}
}

type workflowDataSource struct {
	client *atlassian.Client
}

func (d *workflowDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_jira_workflow"
}

func (d *workflowDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	stringList := func(desc string) schema.ListAttribute {
		return schema.ListAttribute{Description: desc, Computed: true, ElementType: types.StringType}
	}
	resp.Schema = schema.Schema{
		Description: "Use this data source to read a company-managed Jira Cloud workflow by name, including its transitions and transition rules.",
		Attributes: map[string]schema.Attribute{
			"name":                   schema.StringAttribute{Description: "The name of the workflow to look up.", Required: true},
			"id":                     schema.StringAttribute{Description: "The entity ID (UUID) of the workflow.", Computed: true},
			"description":            schema.StringAttribute{Description: "The description of the workflow.", Computed: true},
			"version":                schema.Int64Attribute{Description: "The document version number.", Computed: true},
			"required_field_message": schema.StringAttribute{Description: "Not read back — only meaningful on the resource.", Computed: true},
			"statuses": schema.ListNestedAttribute{
				Description: "Statuses used by the workflow, in layout order.",
				Computed:    true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"status_id": schema.StringAttribute{Description: "The status ID.", Computed: true},
				}},
			},
			"transitions": schema.ListNestedAttribute{
				Description: "Transitions (the initial transition is not listed).",
				Computed:    true,
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"name":                schema.StringAttribute{Description: "Transition name.", Computed: true},
					"type":                schema.StringAttribute{Description: "`DIRECTED` or `GLOBAL`.", Computed: true},
					"from":                stringList("Status IDs the transition starts from."),
					"to":                  schema.StringAttribute{Description: "Status ID the transition leads to.", Computed: true},
					"allowed_groups":      stringList("Group IDs allowed to perform the transition."),
					"allowed_roles":       stringList("Project role IDs allowed to perform the transition."),
					"allowed_account_ids": stringList("Account IDs allowed to perform the transition."),
					"separation_of_duties": schema.ListNestedAttribute{
						Description: "Separation-of-duties rules.",
						Computed:    true,
						NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
							"from": schema.StringAttribute{Description: "Status ID the earlier move started from.", Computed: true},
							"to":   schema.StringAttribute{Description: "Status ID the earlier move led to.", Computed: true},
						}},
					},
					"assign": schema.SingleNestedAttribute{
						Description: "Assignee post function, if any.",
						Computed:    true,
						Attributes: map[string]schema.Attribute{
							"type":       schema.StringAttribute{Description: "Assignee rule type.", Computed: true},
							"account_id": schema.StringAttribute{Description: "Account ID for `to-selected-user`.", Computed: true},
						},
					},
					"required_fields": stringList("Field IDs required by validators."),
				}},
			},
		},
	}
}

func (d *workflowDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *workflowDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config workflowResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	name := config.Name.ValueString()
	doc, refToID, found, err := fetchWorkflow(ctx, d.client, workflowReadRequest{WorkflowNames: []string{name}})
	if err != nil {
		resp.Diagnostics.AddError("Error reading workflow", err.Error())
		return
	}
	if !found || doc.Name != name {
		resp.Diagnostics.AddError("Workflow not found", fmt.Sprintf("No workflow found with name %q", name))
		return
	}

	resp.Diagnostics.Append(modelFromDocument(ctx, &config, doc, refToID)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}

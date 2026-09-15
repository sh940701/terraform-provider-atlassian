package jira

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

var _ datasource.DataSource = &webhookDataSource{}

// NewWebhookDataSource returns a new webhook data source.
func NewWebhookDataSource() datasource.DataSource {
	return &webhookDataSource{}
}

type webhookDataSource struct {
	client *atlassian.Client
}

type webhookDataSourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	URL         types.String `tfsdk:"url"`
	Events      types.Set    `tfsdk:"events"`
	JQL         types.String `tfsdk:"jql"`
	ExcludeBody types.Bool   `tfsdk:"exclude_body"`
	Enabled     types.Bool   `tfsdk:"enabled"`
	IsSigned    types.Bool   `tfsdk:"is_signed"`
}

func (d *webhookDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_jira_webhook"
}

func (d *webhookDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Use this data source to read a Jira Cloud administrator webhook by ID.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Description: "The numeric ID of the webhook.", Required: true},
			"name":         schema.StringAttribute{Description: "The name of the webhook.", Computed: true},
			"description":  schema.StringAttribute{Description: "A description of the webhook.", Computed: true},
			"url":          schema.StringAttribute{Description: "The URL Jira posts events to.", Computed: true},
			"events":       schema.SetAttribute{Description: "Events that trigger the webhook.", Computed: true, ElementType: types.StringType},
			"jql":          schema.StringAttribute{Description: "JQL filter for issue-related events.", Computed: true},
			"exclude_body": schema.BoolAttribute{Description: "Whether the issue body is excluded from deliveries.", Computed: true},
			"enabled":      schema.BoolAttribute{Description: "Whether the webhook is enabled.", Computed: true},
			"is_signed":    schema.BoolAttribute{Description: "Whether a secret is configured.", Computed: true},
		},
	}
}

func (d *webhookDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *webhookDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config webhookDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var doc webhookAPIDocument
	apiPath := "/rest/webhooks/1.0/webhook/" + atlassian.PathEscape(config.ID.ValueString())
	statusCode, err := d.client.GetWithStatus(ctx, apiPath, &doc)
	if err != nil {
		resp.Diagnostics.AddError("Error reading webhook", err.Error())
		return
	}
	if statusCode == http.StatusNotFound {
		resp.Diagnostics.AddError("Webhook not found", fmt.Sprintf("No webhook found with ID %q", config.ID.ValueString()))
		return
	}

	config.Name = types.StringValue(doc.Name)
	config.Description = types.StringValue(doc.Description)
	config.URL = types.StringValue(doc.URL)
	config.JQL = types.StringValue(doc.Filters[webhookJQLFilterKey])
	config.ExcludeBody = types.BoolValue(doc.ExcludeBody)
	config.Enabled = types.BoolValue(doc.Enabled == nil || *doc.Enabled)
	config.IsSigned = types.BoolValue(doc.IsSigned)
	events, diags := types.SetValueFrom(ctx, types.StringType, doc.Events)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	config.Events = events

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}

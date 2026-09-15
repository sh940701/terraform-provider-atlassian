package jira

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

var _ datasource.DataSource = &groupMembersDataSource{}

// NewGroupMembersDataSource returns a new group members data source.
func NewGroupMembersDataSource() datasource.DataSource {
	return &groupMembersDataSource{}
}

type groupMembersDataSource struct {
	client *atlassian.Client
}

type groupMembersDataSourceModel struct {
	GroupID    types.String `tfsdk:"group_id"`
	AccountIDs types.List   `tfsdk:"account_ids"`
}

func (d *groupMembersDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_jira_group_members"
}

func (d *groupMembersDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Use this data source to list the account IDs of every member (active or inactive) of a Jira Cloud group.",
		Attributes: map[string]schema.Attribute{
			"group_id": schema.StringAttribute{
				Description: "The ID of the group.",
				Required:    true,
			},
			"account_ids": schema.ListAttribute{
				Description: "Account IDs of the group's members, in the order Jira returns them.",
				Computed:    true,
				ElementType: types.StringType,
			},
		},
	}
}

func (d *groupMembersDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *groupMembersDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config groupMembersDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	members, found, err := listGroupMembers(ctx, d.client, config.GroupID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error reading group members", err.Error())
		return
	}
	if !found {
		resp.Diagnostics.AddError(
			"Group not found",
			fmt.Sprintf("No group found with ID %q", config.GroupID.ValueString()),
		)
		return
	}

	list, diags := types.ListValueFrom(ctx, types.StringType, members)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	config.AccountIDs = list

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}

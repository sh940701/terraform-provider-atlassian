package jira

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

var (
	_ resource.Resource                = &groupMemberResource{}
	_ resource.ResourceWithImportState = &groupMemberResource{}
)

// NewGroupMemberResource returns a new group member resource.
func NewGroupMemberResource() resource.Resource {
	return &groupMemberResource{}
}

type groupMemberResource struct {
	client *atlassian.Client
}

type groupMemberResourceModel struct {
	ID        types.String `tfsdk:"id"`
	GroupID   types.String `tfsdk:"group_id"`
	AccountID types.String `tfsdk:"account_id"`
}

func groupMemberID(groupID, accountID string) string {
	return groupID + "/" + accountID
}

func (r *groupMemberResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_jira_group_member"
}

func (r *groupMemberResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a single user's membership of a Jira Cloud group. " +
			"Requires site administrator rights (POST/DELETE /rest/api/3/group/user).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "Composite identifier `<group_id>/<account_id>`.",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"group_id": schema.StringAttribute{
				Description: "The ID of the group. Changing this forces recreation of the resource.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"account_id": schema.StringAttribute{
				Description: "The Atlassian account ID of the user. Changing this forces recreation of the resource.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
		},
	}
}

func (r *groupMemberResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *groupMemberResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan groupMemberResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiPath := fmt.Sprintf("/rest/api/3/group/user?groupId=%s", atlassian.QueryEscape(plan.GroupID.ValueString()))
	body := map[string]string{"accountId": plan.AccountID.ValueString()}
	if err := r.client.Post(ctx, apiPath, body, nil); err != nil {
		resp.Diagnostics.AddError("Error adding user to group", err.Error())
		return
	}

	plan.ID = types.StringValue(groupMemberID(plan.GroupID.ValueString(), plan.AccountID.ValueString()))
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *groupMemberResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state groupMemberResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	members, found, err := listGroupMembers(ctx, r.client, state.GroupID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error reading group members", err.Error())
		return
	}
	if !found {
		// The group itself is gone.
		resp.State.RemoveResource(ctx)
		return
	}

	for _, id := range members {
		if id == state.AccountID.ValueString() {
			state.ID = types.StringValue(groupMemberID(state.GroupID.ValueString(), state.AccountID.ValueString()))
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
	}

	// Membership removed out-of-band.
	resp.State.RemoveResource(ctx)
}

func (r *groupMemberResource) Update(_ context.Context, _ resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError(
		"Update not supported",
		"A group membership has no mutable attributes; group_id and account_id force replacement.",
	)
}

func (r *groupMemberResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state groupMemberResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiPath := fmt.Sprintf("/rest/api/3/group/user?groupId=%s&accountId=%s",
		atlassian.QueryEscape(state.GroupID.ValueString()), atlassian.QueryEscape(state.AccountID.ValueString()))
	statusCode, err := r.client.DeleteWithStatus(ctx, apiPath)

	// 404 means the membership (or the group) was already removed out-of-band; treat as success.
	if statusCode == http.StatusNotFound {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Error removing user from group", err.Error())
		return
	}
}

// ImportState accepts `<group_id>/<account_id>`.
func (r *groupMemberResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError(
			"Invalid import ID",
			fmt.Sprintf("Expected `<group_id>/<account_id>`, got %q", req.ID),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("group_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("account_id"), parts[1])...)
}

// listGroupMembers returns every account ID in the group (active and
// inactive — a deactivated user is still a member and must not read as
// drift). found is false when the group does not exist (404).
func listGroupMembers(ctx context.Context, client *atlassian.Client, groupID string) ([]string, bool, error) {
	apiPath := fmt.Sprintf("/rest/api/3/group/member?groupId=%s&includeInactiveUsers=true", atlassian.QueryEscape(groupID))

	rawValues, status, err := client.GetAllPagesWithStatus(ctx, apiPath)
	if err != nil {
		return nil, false, err
	}
	if status == http.StatusNotFound {
		return nil, false, nil
	}

	members := make([]string, 0, len(rawValues))
	for _, raw := range rawValues {
		var u struct {
			AccountID string `json:"accountId"`
		}
		if err := json.Unmarshal(raw, &u); err != nil {
			return nil, true, fmt.Errorf("unmarshaling group member: %w", err)
		}
		members = append(members, u.AccountID)
	}
	return members, true, nil
}

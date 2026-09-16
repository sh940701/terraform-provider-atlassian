package jira

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

var (
	_ resource.Resource                = &permissionSchemeResource{}
	_ resource.ResourceWithImportState = &permissionSchemeResource{}
)

// NewPermissionSchemeResource returns a new permission scheme resource.
func NewPermissionSchemeResource() resource.Resource {
	return &permissionSchemeResource{}
}

type permissionSchemeResource struct {
	client *atlassian.Client
}

type permissionSchemeResourceModel struct {
	ID                types.String `tfsdk:"id"`
	Name              types.String `tfsdk:"name"`
	Description       types.String `tfsdk:"description"`
	KeepDefaultGrants types.Bool   `tfsdk:"keep_default_grants"`
}

// permissionSchemeAPIResponse represents the Jira permission scheme API response shape.
type permissionSchemeAPIResponse struct {
	ID          int                     `json:"id"`
	Name        string                  `json:"name"`
	Description string                  `json:"description,omitempty"`
	Permissions []permissionSchemeGrant `json:"permissions,omitempty"`
}

// permissionSchemeGrant is one grant as returned with expand=permissions.
type permissionSchemeGrant struct {
	ID     int `json:"id"`
	Holder struct {
		Type      string `json:"type"`
		Parameter string `json:"parameter"`
	} `json:"holder"`
}

// addonsProjectRole is the project role Jira requires for Connect apps; its
// seeded grants are the only ones kept when a new scheme is emptied.
const addonsProjectRole = "atlassian-addons-project-access"

// permissionSchemeCreateRequest represents the POST/PUT request body.
type permissionSchemeCreateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

func (r *permissionSchemeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_jira_permission_scheme"
}

func (r *permissionSchemeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a Jira Cloud permission scheme.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "The ID of the permission scheme.",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Description: "The name of the permission scheme.",
				Required:    true,
			},
			"description": schema.StringAttribute{
				Description: "The description of the permission scheme.",
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(""),
			},
			"keep_default_grants": schema.BoolAttribute{
				Description: "Jira seeds every new permission scheme with its default grants (dozens, including DELETE_* for the administrators and guest roles). By default the provider removes them right after creation so the scheme holds only the grants declared as `atlassian_jira_permission_scheme_grant` resources — except the `atlassian-addons-project-access` role grants Jira requires for apps. Set true to keep Jira's defaults.",
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
			},
		},
	}
}

func (r *permissionSchemeResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *permissionSchemeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan permissionSchemeResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := permissionSchemeCreateRequest{
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
	}

	var result permissionSchemeAPIResponse
	err := r.client.Post(ctx, "/rest/api/3/permissionscheme", body, &result)
	if err != nil {
		resp.Diagnostics.AddError("Error creating permission scheme", err.Error())
		return
	}

	plan.ID = types.StringValue(fmt.Sprintf("%d", result.ID))
	plan.Name = types.StringValue(result.Name)
	plan.Description = types.StringValue(result.Description)

	if !plan.KeepDefaultGrants.ValueBool() {
		if err := r.pruneSeededGrants(ctx, plan.ID.ValueString()); err != nil {
			resp.Diagnostics.AddError("Error removing Jira's default grants from the new permission scheme", err.Error())
			return
		}
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// pruneSeededGrants deletes the grants Jira seeded into a freshly created
// scheme, keeping only those of the atlassian-addons-project-access role.
func (r *permissionSchemeResource) pruneSeededGrants(ctx context.Context, schemeID string) error {
	var roles []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := r.client.Get(ctx, "/rest/api/3/role", &roles); err != nil {
		return fmt.Errorf("listing project roles: %w", err)
	}
	addons := ""
	for _, role := range roles {
		if role.Name == addonsProjectRole {
			addons = fmt.Sprint(role.ID)
		}
	}
	var scheme permissionSchemeAPIResponse
	if err := r.client.Get(ctx, "/rest/api/3/permissionscheme/"+atlassian.PathEscape(schemeID)+"?expand=permissions", &scheme); err != nil {
		return fmt.Errorf("reading seeded grants: %w", err)
	}
	for _, g := range scheme.Permissions {
		if g.Holder.Type == "projectRole" && g.Holder.Parameter == addons {
			continue
		}
		if err := r.client.Delete(ctx, fmt.Sprintf("/rest/api/3/permissionscheme/%s/permission/%d", atlassian.PathEscape(schemeID), g.ID)); err != nil {
			return fmt.Errorf("deleting seeded grant %d: %w", g.ID, err)
		}
	}
	return nil
}

func (r *permissionSchemeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state permissionSchemeResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var result permissionSchemeAPIResponse
	apiPath := fmt.Sprintf("/rest/api/3/permissionscheme/%s", atlassian.PathEscape(state.ID.ValueString()))
	statusCode, err := r.client.GetWithStatus(ctx, apiPath, &result)
	if err != nil {
		resp.Diagnostics.AddError("Error reading permission scheme", err.Error())
		return
	}

	if statusCode == http.StatusNotFound {
		resp.State.RemoveResource(ctx)
		return
	}

	state.ID = types.StringValue(fmt.Sprintf("%d", result.ID))
	state.Name = types.StringValue(result.Name)
	state.Description = types.StringValue(result.Description)

	// Imported or pre-0.2.7 state has no keep_default_grants; the schema default applies.
	if state.KeepDefaultGrants.IsNull() || state.KeepDefaultGrants.IsUnknown() {
		state.KeepDefaultGrants = types.BoolValue(false)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *permissionSchemeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan permissionSchemeResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := permissionSchemeCreateRequest{
		Name:        plan.Name.ValueString(),
		Description: plan.Description.ValueString(),
	}

	var result permissionSchemeAPIResponse
	apiPath := fmt.Sprintf("/rest/api/3/permissionscheme/%s", atlassian.PathEscape(plan.ID.ValueString()))
	err := r.client.Put(ctx, apiPath, body, &result)
	if err != nil {
		resp.Diagnostics.AddError("Error updating permission scheme", err.Error())
		return
	}

	plan.ID = types.StringValue(fmt.Sprintf("%d", result.ID))
	plan.Name = types.StringValue(result.Name)
	plan.Description = types.StringValue(result.Description)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *permissionSchemeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state permissionSchemeResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiPath := fmt.Sprintf("/rest/api/3/permissionscheme/%s", atlassian.PathEscape(state.ID.ValueString()))
	statusCode, err := r.client.DeleteWithStatus(ctx, apiPath)
	if err != nil {
		resp.Diagnostics.AddError("Error deleting permission scheme", err.Error())
		return
	}

	// 404 means the scheme was already deleted out-of-band; treat as success.
	if statusCode == http.StatusNotFound {
		return
	}
}

func (r *permissionSchemeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	schemeID := req.ID

	var result permissionSchemeAPIResponse
	apiPath := fmt.Sprintf("/rest/api/3/permissionscheme/%s", atlassian.PathEscape(schemeID))
	statusCode, err := r.client.GetWithStatus(ctx, apiPath, &result)
	if err != nil {
		resp.Diagnostics.AddError("Error importing permission scheme", err.Error())
		return
	}

	if statusCode == http.StatusNotFound {
		resp.Diagnostics.AddError(
			"Permission scheme not found",
			fmt.Sprintf("No permission scheme found with ID %q", schemeID),
		)
		return
	}

	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), fmt.Sprintf("%d", result.ID))...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), result.Name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("description"), result.Description)...)
}

package jira

import (
	"context"
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
	_ resource.Resource                = &screenTabFieldOrderResource{}
	_ resource.ResourceWithImportState = &screenTabFieldOrderResource{}
)

// NewScreenTabFieldOrderResource returns a resource that orders the fields on a screen tab.
func NewScreenTabFieldOrderResource() resource.Resource {
	return &screenTabFieldOrderResource{}
}

type screenTabFieldOrderResource struct {
	client *atlassian.Client
}

type screenTabFieldOrderResourceModel struct {
	ID       types.String `tfsdk:"id"`
	ScreenID types.String `tfsdk:"screen_id"`
	TabID    types.String `tfsdk:"tab_id"`
	FieldIDs types.List   `tfsdk:"field_ids"`
}

func (r *screenTabFieldOrderResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_jira_screen_tab_field_order"
}

func (r *screenTabFieldOrderResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Orders fields on a Jira Cloud screen tab. The listed fields are placed first, in the given order; " +
			"fields on the tab that are not listed keep their relative order after them. The fields must already be on the tab " +
			"(see `atlassian_jira_screen_tab_field`). Jira's new issue view shows the «Details» group in tab order, so this " +
			"controls what people see. Deleting the resource leaves the current order in place.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "`{screen_id}/{tab_id}`.",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"screen_id": schema.StringAttribute{
				Description: "The ID of the screen. Changing this forces recreation of the resource.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"tab_id": schema.StringAttribute{
				Description: "The ID of the screen tab. Changing this forces recreation of the resource.",
				Required:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"field_ids": schema.ListAttribute{
				Description: "Field IDs in the order they should appear at the top of the tab, e.g. `[\"summary\", \"customfield_10001\"]`.",
				Required:    true,
				ElementType: types.StringType,
			},
		},
	}
}

func (r *screenTabFieldOrderResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*atlassian.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected Resource Configure Type", fmt.Sprintf("Expected *atlassian.Client, got: %T", req.ProviderData))
		return
	}
	r.client = client
}

func (r *screenTabFieldOrderResource) fieldsPath(screenID, tabID string) string {
	return fmt.Sprintf("/rest/api/3/screens/%s/tabs/%s/fields", atlassian.PathEscape(screenID), atlassian.PathEscape(tabID))
}

// applyOrder moves the listed fields to the front, one after another: first → "First", then each after its predecessor.
// Jira exposes no bulk reorder — n moves for n listed fields, under the provider-wide config lock like other screen writes.
func (r *screenTabFieldOrderResource) applyOrder(ctx context.Context, screenID, tabID string, ids []string) error {
	base := r.fieldsPath(screenID, tabID)
	for i, id := range ids {
		body := map[string]string{"position": "First"}
		if i > 0 {
			body = map[string]string{"after": ids[i-1]}
		}
		movePath := base + "/" + atlassian.PathEscape(id) + "/move"
		var status int
		err := retryOnConflict(ctx, func() (int, error) {
			var err error
			status, err = r.client.PostWithStatus(ctx, movePath, body, nil)
			return status, err
		})
		if err != nil {
			return fmt.Errorf("moving field %s: %w", id, err)
		}
		if status == http.StatusNotFound {
			return fmt.Errorf("field %s is not on screen %s tab %s — add it with atlassian_jira_screen_tab_field first", id, screenID, tabID)
		}
	}
	return nil
}

// currentOrder returns the tab's fields in Jira order; ok=false when the tab no longer exists.
func (r *screenTabFieldOrderResource) currentOrder(ctx context.Context, screenID, tabID string) (order []string, ok bool, err error) {
	var fields []screenTabFieldAPIResponse
	status, err := r.client.GetWithStatus(ctx, r.fieldsPath(screenID, tabID), &fields)
	if err != nil {
		return nil, false, err
	}
	if status == http.StatusNotFound {
		return nil, false, nil
	}
	for _, f := range fields {
		order = append(order, f.ID)
	}
	return order, true, nil
}

func listToStrings(ctx context.Context, l types.List) ([]string, error) {
	var out []string
	diags := l.ElementsAs(ctx, &out, false)
	if diags.HasError() {
		return nil, fmt.Errorf("field_ids: %s", diags.Errors()[0].Detail())
	}
	return out, nil
}

func (r *screenTabFieldOrderResource) write(ctx context.Context, plan *screenTabFieldOrderResourceModel) error {
	ids, err := listToStrings(ctx, plan.FieldIDs)
	if err != nil {
		return err
	}
	return withConfigLock(func() error { return r.applyOrder(ctx, plan.ScreenID.ValueString(), plan.TabID.ValueString(), ids) })
}

func (r *screenTabFieldOrderResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan screenTabFieldOrderResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.write(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("Error ordering screen tab fields", err.Error())
		return
	}
	plan.ID = types.StringValue(plan.ScreenID.ValueString() + "/" + plan.TabID.ValueString())
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *screenTabFieldOrderResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state screenTabFieldOrderResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	order, ok, err := r.currentOrder(ctx, state.ScreenID.ValueString(), state.TabID.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Error reading screen tab fields", err.Error())
		return
	}
	if !ok {
		resp.State.RemoveResource(ctx)
		return
	}
	listed, err := listToStrings(ctx, state.FieldIDs)
	if err != nil {
		resp.Diagnostics.AddError("Error reading state", err.Error())
		return
	}
	// state = the listed fields as Jira orders them now — drift (someone dragged a field in the UI) shows in plan.
	want := make(map[string]bool, len(listed))
	for _, id := range listed {
		want[id] = true
	}
	now := make([]string, 0, len(listed))
	for _, id := range order {
		if want[id] {
			now = append(now, id)
		}
	}
	// Fields that vanished from the tab stay in state so the plan says "put it back" instead of silently forgetting them.
	seen := make(map[string]bool, len(now))
	for _, id := range now {
		seen[id] = true
	}
	for _, id := range listed {
		if !seen[id] {
			now = append(now, id)
		}
	}
	lv, diags := types.ListValueFrom(ctx, types.StringType, now)
	resp.Diagnostics.Append(diags...)
	state.FieldIDs = lv
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *screenTabFieldOrderResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan screenTabFieldOrderResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.write(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("Error ordering screen tab fields", err.Error())
		return
	}
	plan.ID = types.StringValue(plan.ScreenID.ValueString() + "/" + plan.TabID.ValueString())
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *screenTabFieldOrderResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
	// Order is not a thing Jira can "remove" — the tab keeps whatever order it has. Only the state entry goes away.
}

func (r *screenTabFieldOrderResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import format: {screenId}/{tabId} — field_ids becomes the tab's current full order.
	parts := strings.SplitN(req.ID, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		resp.Diagnostics.AddError("Invalid import ID", fmt.Sprintf("Expected import ID in the format {screen_id}/{tab_id}, got %q", req.ID))
		return
	}
	order, ok, err := r.currentOrder(ctx, parts[0], parts[1])
	if err != nil {
		resp.Diagnostics.AddError("Error reading screen tab fields", err.Error())
		return
	}
	if !ok {
		resp.Diagnostics.AddError("Screen tab not found", fmt.Sprintf("screen %s tab %s", parts[0], parts[1]))
		return
	}
	lv, diags := types.ListValueFrom(ctx, types.StringType, order)
	resp.Diagnostics.Append(diags...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("screen_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("tab_id"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("field_ids"), lv)...)
}

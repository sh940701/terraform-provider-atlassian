package jira

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/lbajsarowicz/terraform-provider-atlassian/internal/atlassian"
)

var (
	_ resource.Resource                = &webhookResource{}
	_ resource.ResourceWithImportState = &webhookResource{}
	_ resource.ResourceWithModifyPlan  = &webhookResource{}
)

// NewWebhookResource returns a new webhook resource.
func NewWebhookResource() resource.Resource {
	return &webhookResource{}
}

type webhookResource struct {
	client *atlassian.Client
}

type webhookResourceModel struct {
	ID          types.String `tfsdk:"id"`
	Name        types.String `tfsdk:"name"`
	Description types.String `tfsdk:"description"`
	URL         types.String `tfsdk:"url"`
	Events      types.Set    `tfsdk:"events"`
	JQL         types.String `tfsdk:"jql"`
	ExcludeBody types.Bool   `tfsdk:"exclude_body"`
	Enabled     types.Bool   `tfsdk:"enabled"`
	Secret      types.String `tfsdk:"secret"`
	IsSigned    types.Bool   `tfsdk:"is_signed"`
}

const webhookJQLFilterKey = "issue-related-events-section"

// webhookAPIDocument is the administrator webhook document
// (/rest/webhooks/1.0/webhook). `secret` is write-only: it is accepted on
// POST/PUT and never returned; `isSigned` reports whether one is set.
type webhookAPIDocument struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	URL         string            `json:"url"`
	Events      []string          `json:"events"`
	Filters     map[string]string `json:"filters"`
	ExcludeBody bool              `json:"excludeBody"`
	Enabled     *bool             `json:"enabled,omitempty"`
	Secret      *string           `json:"secret,omitempty"`
	Self        string            `json:"self,omitempty"`
	IsSigned    bool              `json:"isSigned,omitempty"`
}

func (r *webhookResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_jira_webhook"
}

func (r *webhookResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages a Jira Cloud administrator webhook (/rest/webhooks/1.0/webhook). " +
			"Requires Jira administrator rights and a classic API token (scoped tokens are rejected). " +
			"The webhook URL must be HTTPS on a port Atlassian allows.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Description: "The numeric ID of the webhook.",
				Computed:    true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Description: "The name of the webhook.",
				Required:    true,
			},
			"description": schema.StringAttribute{
				Description: "A description of the webhook.",
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(""),
			},
			"url": schema.StringAttribute{
				Description: "The HTTPS URL Jira posts events to. Allowed ports: 443, 1880-1890, 4044, 6017, 7990, 8060, 8080, 8085, 8089, 8090, 8443, 8444, 8900, 9900, 9420, 9520.",
				Required:    true,
				Validators:  []validator.String{webhookURLValidator{}},
			},
			"events": schema.SetAttribute{
				Description: "Events that trigger the webhook, e.g. `jira:issue_created`, `jira:issue_updated`, `comment_created`. Order is not significant (Jira returns its own).",
				Required:    true,
				ElementType: types.StringType,
				Validators:  []validator.Set{setvalidator.SizeAtLeast(1)},
			},
			"jql": schema.StringAttribute{
				Description: "JQL filter for issue-related events (the `issue-related-events-section` filter). Empty means all issues.",
				Optional:    true,
				Computed:    true,
				Default:     stringdefault.StaticString(""),
			},
			"exclude_body": schema.BoolAttribute{
				Description: "When true, Jira sends the event without the issue body.",
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(false),
			},
			"enabled": schema.BoolAttribute{
				Description: "Whether the webhook is enabled. Disabling it in the Jira UI shows up as drift.",
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
			},
			"secret": schema.StringAttribute{
				Description: "Secret used to sign deliveries (X-Hub-Signature). Jira never returns it; the configured value is kept in state. Set to an empty string to remove it.",
				Optional:    true,
				Computed:    true,
				Sensitive:   true,
				Default:     stringdefault.StaticString(""),
			},
			"is_signed": schema.BoolAttribute{
				Description: "Whether Jira reports a secret as configured. Compare with `secret` to detect out-of-band changes.",
				Computed:    true,
			},
		},
	}
}

func (r *webhookResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// ModifyPlan keeps `is_signed` known during updates: Jira reports it as
// "a secret is set", which follows directly from the planned `secret`.
func (r *webhookResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy
	}
	var secret types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("secret"), &secret)...)
	if resp.Diagnostics.HasError() || secret.IsUnknown() {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("is_signed"), types.BoolValue(secret.ValueString() != ""))...)
}

func (r *webhookResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan webhookResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, diags := webhookDocumentFromModel(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if s := plan.Secret.ValueString(); s != "" {
		body.Secret = &s
	}

	var result webhookAPIDocument
	if err := r.client.Post(ctx, "/rest/webhooks/1.0/webhook", body, &result); err != nil {
		resp.Diagnostics.AddError("Error creating webhook", err.Error())
		return
	}

	id, err := webhookIDFromSelf(result.Self)
	if err != nil {
		resp.Diagnostics.AddError("Error creating webhook", err.Error())
		return
	}

	// Preserve plan values; take only server-assigned facts from the response.
	plan.ID = types.StringValue(id)
	plan.IsSigned = types.BoolValue(result.IsSigned)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *webhookResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state webhookResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var doc webhookAPIDocument
	apiPath := "/rest/webhooks/1.0/webhook/" + atlassian.PathEscape(state.ID.ValueString())
	statusCode, err := r.client.GetWithStatus(ctx, apiPath, &doc)
	if err != nil {
		resp.Diagnostics.AddError("Error reading webhook", err.Error())
		return
	}
	if statusCode == http.StatusNotFound {
		resp.State.RemoveResource(ctx)
		return
	}

	resp.Diagnostics.Append(webhookModelFromDocument(ctx, &state, doc)...)
	if resp.Diagnostics.HasError() {
		return
	}
	switch {
	case doc.IsSigned && state.Secret.ValueString() == "":
		resp.Diagnostics.AddWarning("Webhook secret unknown",
			fmt.Sprintf("Jira reports webhook %s as signed but no `secret` is configured (imported, or set outside Terraform). Set `secret` to the value Jira has, or to \"\" to remove it on the next apply.", state.ID.ValueString()))
	case !doc.IsSigned && state.Secret.ValueString() != "":
		resp.Diagnostics.AddWarning("Webhook secret removed outside Terraform",
			fmt.Sprintf("Webhook %s is no longer signed although `secret` is configured; the next apply re-sends it.", state.ID.ValueString()))
		state.Secret = types.StringValue("") // makes the plan re-send the configured secret
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *webhookResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state webhookResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body, diags := webhookDocumentFromModel(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Omitted secret = keep. Send it only when the configured value changed;
	// an empty string tells Jira to remove it.
	if plan.Secret.ValueString() != state.Secret.ValueString() {
		s := plan.Secret.ValueString()
		body.Secret = &s
	}

	// The PUT response body is undocumented (may be empty): do not decode it,
	// read the webhook back instead.
	apiPath := "/rest/webhooks/1.0/webhook/" + atlassian.PathEscape(state.ID.ValueString())
	if err := r.client.Put(ctx, apiPath, body, nil); err != nil {
		resp.Diagnostics.AddError("Error updating webhook", err.Error())
		return
	}
	var doc webhookAPIDocument
	if _, err := r.client.GetWithStatus(ctx, apiPath, &doc); err != nil {
		resp.Diagnostics.AddError("Error reading webhook after update", err.Error())
		return
	}

	plan.ID = state.ID
	plan.IsSigned = types.BoolValue(doc.IsSigned)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *webhookResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state webhookResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiPath := "/rest/webhooks/1.0/webhook/" + atlassian.PathEscape(state.ID.ValueString())
	statusCode, err := r.client.DeleteWithStatus(ctx, apiPath)

	// 404 means the webhook was already deleted out-of-band; treat as success.
	if statusCode == http.StatusNotFound {
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Error deleting webhook", err.Error())
		return
	}
}

func (r *webhookResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// webhookDocumentFromModel builds the request body (without secret).
func webhookDocumentFromModel(ctx context.Context, m webhookResourceModel) (webhookAPIDocument, diag.Diagnostics) {
	var events []string
	diags := m.Events.ElementsAs(ctx, &events, false)
	enabled := m.Enabled.ValueBool()
	doc := webhookAPIDocument{
		Name:        m.Name.ValueString(),
		Description: m.Description.ValueString(),
		URL:         m.URL.ValueString(),
		Events:      events,
		Filters:     map[string]string{webhookJQLFilterKey: m.JQL.ValueString()},
		ExcludeBody: m.ExcludeBody.ValueBool(),
		Enabled:     &enabled,
	}
	return doc, diags
}

// webhookModelFromDocument copies every readable attribute from the API
// document into the model. `secret` is left as-is (never returned).
func webhookModelFromDocument(ctx context.Context, m *webhookResourceModel, doc webhookAPIDocument) diag.Diagnostics {
	m.Name = types.StringValue(doc.Name)
	m.Description = types.StringValue(doc.Description)
	m.URL = types.StringValue(doc.URL)
	m.JQL = types.StringValue(doc.Filters[webhookJQLFilterKey])
	m.ExcludeBody = types.BoolValue(doc.ExcludeBody)
	m.Enabled = types.BoolValue(doc.Enabled == nil || *doc.Enabled)
	m.IsSigned = types.BoolValue(doc.IsSigned)
	if m.Secret.IsNull() || m.Secret.IsUnknown() {
		m.Secret = types.StringValue("")
	}
	events, diags := types.SetValueFrom(ctx, types.StringType, doc.Events)
	m.Events = events
	return diags
}

func webhookIDFromSelf(self string) (string, error) {
	i := strings.LastIndex(self, "/")
	if i < 0 || i == len(self)-1 {
		return "", fmt.Errorf("cannot derive webhook id from self link %q", self)
	}
	return self[i+1:], nil
}

// webhookURLValidator enforces Atlassian's rules for administrator webhook
// URLs: HTTPS only, and only the documented ports.
type webhookURLValidator struct{}

var webhookAllowedPorts = map[int]bool{
	443: true, 4044: true, 6017: true, 7990: true, 8060: true, 8080: true, 8085: true, 8089: true,
	8090: true, 8443: true, 8444: true, 8900: true, 9900: true, 9420: true, 9520: true,
}

func (webhookURLValidator) Description(_ context.Context) string {
	return "must be an https:// URL on a port Jira allows for webhooks"
}

func (v webhookURLValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (webhookURLValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	raw := req.ConfigValue.ValueString()
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		resp.Diagnostics.AddAttributeError(req.Path, "Invalid webhook URL",
			fmt.Sprintf("Jira only accepts https:// webhook URLs, got %q", raw))
		return
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || (!webhookAllowedPorts[n] && (n < 1880 || n > 1890)) {
			resp.Diagnostics.AddAttributeError(req.Path, "Invalid webhook URL port",
				fmt.Sprintf("Jira does not deliver webhooks to port %s. Allowed: 443, 1880-1890, 4044, 6017, 7990, 8060, 8080, 8085, 8089, 8090, 8443, 8444, 8900, 9900, 9420, 9520.", p))
		}
	}
}

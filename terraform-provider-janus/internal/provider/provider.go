// Package provider is the Janus Terraform provider: the janus_node and
// janus_hypervisor resources and the janus_hypervisor data source, on a
// Janus Controller's API (internal/client).
package provider

import (
	"context"
	"os"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/swenske/Janus/terraform-provider-janus/internal/client"
)

type janusProvider struct {
	version string
}

func New(version string) func() provider.Provider {
	return func() provider.Provider { return &janusProvider{version: version} }
}

type providerModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Token    types.String `tfsdk:"token"`
	CACert   types.String `tfsdk:"ca_cert"`
	Insecure types.Bool   `tfsdk:"insecure"`
}

func (p *janusProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "janus"
	resp.Version = p.version
}

func (p *janusProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Janus nodes on a Janus Controller's hypervisors.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional:    true,
				Description: "The Controller's URL, https://host[:port]. Default: $JANUS_ENDPOINT.",
			},
			"token": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "An API token (the Controller's API tokens tab). Default: $JANUS_TOKEN.",
			},
			"ca_cert": schema.StringAttribute{
				Optional:    true,
				Description: "The certificate to check the Controller's against, PEM - its own, from its Provision panel or GET /api/controller-info - or a file holding it. Default: $JANUS_CA_CERT, else the system's trust store.",
			},
			"insecure": schema.BoolAttribute{
				Optional:    true,
				Description: "Don't check the Controller's certificate. For a throwaway test only: the API token goes to whoever answers.",
			},
		},
	}
}

func (p *janusProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	pick := func(v types.String, env string) string {
		if !v.IsNull() && !v.IsUnknown() {
			return v.ValueString()
		}
		return os.Getenv(env)
	}
	endpoint, token, ca := pick(cfg.Endpoint, "JANUS_ENDPOINT"), pick(cfg.Token, "JANUS_TOKEN"), pick(cfg.CACert, "JANUS_CA_CERT")
	if ca != "" && !strings.HasPrefix(strings.TrimSpace(ca), "-----BEGIN") {
		raw, err := os.ReadFile(ca)
		if err != nil {
			resp.Diagnostics.AddError("ca_cert", "Neither a PEM certificate nor a readable file: "+err.Error())
			return
		}
		ca = string(raw)
	}
	c, err := client.New(endpoint, token, ca, cfg.Insecure.ValueBool())
	if err != nil {
		resp.Diagnostics.AddError("Janus provider configuration", err.Error())
		return
	}
	resp.ResourceData = c
	resp.DataSourceData = c
}

func (p *janusProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{NewNodeResource, NewHypervisorResource, NewProxmoxHypervisorResource}
}

func (p *janusProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{NewHypervisorDataSource}
}

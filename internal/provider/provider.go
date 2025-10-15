// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	itypes "go.podman.io/image/v5/types"
)

var _ provider.Provider = &ContainersProvider{}

const (
	SignatureMethodInsecure       = "insecureAcceptAnything"
	SignatureMethodReject         = "reject"
	SignatureMethodSignedBy       = "signedBy"
	SignatureMethodSigstoreSigned = "sigstoreSigned"
)

type ContainersProvider struct {
	version string
}

type ContainersProviderModel struct {
	SignaturePolicy *SignaturePolicyBlock `tfsdk:"signature_policy"`
	Registries      []RegistryAuthBlock   `tfsdk:"registry"`
}

type SignaturePolicyBlock struct {
	Method           types.String `tfsdk:"method"`
	SignatureKeyPath types.String `tfsdk:"signature_key_path"`
}

// ProviderConfig is passed to resources via ProviderData.
type ProviderConfig struct {
	AuthMap          RegistryAuthMap
	SignatureMethod  string // one of the SignatureMethod* constants
	SignatureKeyPath string // path to GPG/cosign public key; required for signedBy and sigstoreSigned
}

// RegistryConfig holds per-registry auth and transport settings.
type RegistryConfig struct {
	Auth                  *itypes.DockerAuthConfig
	InsecureSkipTLSVerify bool
}

type RegistryAuthBlock struct {
	Hostname              types.String `tfsdk:"hostname"`
	Username              types.String `tfsdk:"username"`
	Password              types.String `tfsdk:"password"`
	InsecureSkipTLSVerify types.Bool   `tfsdk:"insecure_skip_tls_verify"`
}

// RegistryAuthMap maps registry hostname to registry configuration.
type RegistryAuthMap map[string]*RegistryConfig

func (p *ContainersProvider) Metadata(ctx context.Context, req provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "containers"
	resp.Version = p.version
}

func (p *ContainersProvider) Schema(ctx context.Context, req provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Provider for copying container images between registries.",
		Attributes:          map[string]schema.Attribute{},
		Blocks: map[string]schema.Block{
			"signature_policy": schema.SingleNestedBlock{
				MarkdownDescription: "Configure image signature verification policy. " +
					"If omitted, all images are accepted without verification (`insecureAcceptAnything`).",
				Attributes: map[string]schema.Attribute{
					"method": schema.StringAttribute{
						MarkdownDescription: "Signature verification method. Supported values: " +
							"`insecureAcceptAnything` (default), `reject`, `signedBy`, `sigstoreSigned`.",
						Optional: true,
					},
					"signature_key_path": schema.StringAttribute{
						MarkdownDescription: "Path to a public key file for signature verification. " +
							"GPG public key for `signedBy`, cosign public key for `sigstoreSigned`. " +
							"Required when method is `signedBy` or `sigstoreSigned`.",
						Optional: true,
					},
				},
			},
			"registry": schema.ListNestedBlock{
				MarkdownDescription: "Registry authentication credentials. Can be specified multiple times for different registries.",
				NestedObject: schema.NestedBlockObject{
					Attributes: map[string]schema.Attribute{
						"hostname": schema.StringAttribute{
							MarkdownDescription: "Registry hostname (e.g. `docker.io`, `ghcr.io`, `123456789012.dkr.ecr.us-east-1.amazonaws.com`)",
							Required:            true,
						},
						"username": schema.StringAttribute{
							MarkdownDescription: "Username for registry authentication",
							Optional:            true,
						},
						"password": schema.StringAttribute{
							MarkdownDescription: "Password or token for registry authentication",
							Optional:            true,
							Sensitive:           true,
						},
						"insecure_skip_tls_verify": schema.BoolAttribute{
							MarkdownDescription: "Skip TLS certificate verification for this registry. " +
								"Useful for registries with self-signed certificates or HTTP-only registries.",
							Optional: true,
						},
					},
				},
			},
		},
	}
}

func (p *ContainersProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data ContainersProviderModel

	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	authMap := make(RegistryAuthMap)
	for _, r := range data.Registries {
		hostname := r.Hostname.ValueString()
		if _, exists := authMap[hostname]; exists {
			resp.Diagnostics.AddError("Duplicate Registry", "Registry hostname "+hostname+" is configured more than once.")
			return
		}
		rc := &RegistryConfig{
			InsecureSkipTLSVerify: r.InsecureSkipTLSVerify.ValueBool(),
		}
		if !r.Username.IsNull() && !r.Password.IsNull() {
			rc.Auth = &itypes.DockerAuthConfig{
				Username: r.Username.ValueString(),
				Password: r.Password.ValueString(),
			}
		}
		authMap[hostname] = rc
	}

	cfg := &ProviderConfig{
		AuthMap:         authMap,
		SignatureMethod: SignatureMethodInsecure,
	}

	if b := data.SignaturePolicy; b != nil {
		method := SignatureMethodInsecure
		if !b.Method.IsNull() && !b.Method.IsUnknown() {
			method = b.Method.ValueString()
		}

		switch method {
		case SignatureMethodInsecure, SignatureMethodReject:
			// no key needed
		case SignatureMethodSignedBy, SignatureMethodSigstoreSigned:
			if b.SignatureKeyPath.IsNull() || b.SignatureKeyPath.IsUnknown() || b.SignatureKeyPath.ValueString() == "" {
				resp.Diagnostics.AddError("Invalid Signature Policy",
					fmt.Sprintf("signature_key_path is required when method is %q.", method))
				return
			}
			keyPath := b.SignatureKeyPath.ValueString()
			if _, err := os.Stat(keyPath); err != nil {
				resp.Diagnostics.AddError("Invalid Signature Policy",
					fmt.Sprintf("signature_key_path %q is not accessible: %s", keyPath, err))
				return
			}
			cfg.SignatureKeyPath = keyPath
		default:
			resp.Diagnostics.AddError("Invalid Signature Policy",
				fmt.Sprintf("Unsupported method %q. Must be one of: %s, %s, %s, %s.",
					method, SignatureMethodInsecure, SignatureMethodReject, SignatureMethodSignedBy, SignatureMethodSigstoreSigned))
			return
		}
		cfg.SignatureMethod = method
	}

	resp.DataSourceData = cfg
	resp.ResourceData = cfg
}

func (p *ContainersProvider) Resources(ctx context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewImageCopyResource,
	}
}

func (p *ContainersProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return nil
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &ContainersProvider{
			version: version,
		}
	}
}

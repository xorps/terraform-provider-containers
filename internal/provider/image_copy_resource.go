// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/distribution/reference"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/manifest"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/transports/alltransports"
	itypes "go.podman.io/image/v5/types"
)

var _ resource.Resource = &ImageCopyResource{}
var _ resource.ResourceWithValidateConfig = &ImageCopyResource{}

type ImageCopyResource struct {
	config *ProviderConfig
}

func NewImageCopyResource() resource.Resource {
	return &ImageCopyResource{}
}

type ImageCopyResourceModel struct {
	Source            types.String `tfsdk:"source"`
	Destination       types.String `tfsdk:"destination"`
	ArchTags          types.Map    `tfsdk:"arch_tags"`
	StrictArchTags    types.Bool   `tfsdk:"strict_arch_tags"`
	StrictManifest    types.Bool   `tfsdk:"strict_manifest"`
	SourceDigest      types.String `tfsdk:"source_digest"`
	DestinationDigest types.String `tfsdk:"destination_digest"`
	ArchDigests       types.Map    `tfsdk:"arch_digests"`
	Id                types.String `tfsdk:"id"`
}

// manifestListEntry is used to unmarshal entries from both OCI image indexes and Docker manifest lists.
type manifestListEntry struct {
	Digest   string `json:"digest"`
	Platform struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant,omitempty"`
	} `json:"platform"`
}

type genericManifestList struct {
	SchemaVersion int                 `json:"schemaVersion"`
	MediaType     string              `json:"mediaType,omitempty"`
	Manifests     []manifestListEntry `json:"manifests"`
}

func (r *ImageCopyResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_image_copy"
}

func (r *ImageCopyResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Copies a container image from a source registry to a destination registry.",
		Attributes: map[string]schema.Attribute{
			"source": schema.StringAttribute{
				MarkdownDescription: "The source image reference (e.g., `registry/repo:tag`)",
				Required:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"destination": schema.StringAttribute{
				MarkdownDescription: "The destination image reference (e.g., `registry/repo:tag`)",
				Required:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"arch_tags": schema.MapAttribute{
				ElementType:         types.StringType,
				Optional:            true,
				MarkdownDescription: "Map of OCI platform string (e.g. `linux/amd64`) to the final remote tag for that architecture.",
				PlanModifiers:       []planmodifier.Map{mapplanmodifier.RequiresReplace()},
			},
			"strict_arch_tags": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				MarkdownDescription: "If true, fail when a platform specified in `arch_tags` is not found in the source manifest list.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"strict_manifest": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				MarkdownDescription: "If true, fail when `arch_tags` is specified but the source image is not a manifest list.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
			},
			"source_digest": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The digest of the source image at time of copy.",
			},
			"destination_digest": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The digest of the copied manifest at the destination.",
			},
			"arch_digests": schema.MapAttribute{
				ElementType:         types.StringType,
				Computed:            true,
				MarkdownDescription: "Map of platform to digest for each copied arch image.",
			},
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource identifier (set to destination image)",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *ImageCopyResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	cfg, ok := req.ProviderData.(*ProviderConfig)
	if !ok {
		return
	}
	r.config = cfg
}

func (r *ImageCopyResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var data ImageCopyResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !data.ArchTags.IsNull() && !data.ArchTags.IsUnknown() {
		var archTags map[string]string
		if diags := data.ArchTags.ElementsAs(ctx, &archTags, false); diags.HasError() {
			resp.Diagnostics.Append(diags...)
			return
		}
		for platform, tag := range archTags {
			if tag == "" {
				resp.Diagnostics.AddError("Invalid arch_tags",
					fmt.Sprintf("Tag value for platform %q must not be empty.", platform))
			}
			parts := strings.SplitN(platform, "/", 3)
			if len(parts) < 2 {
				resp.Diagnostics.AddError("Invalid arch_tags",
					fmt.Sprintf("Platform key %q must be in os/arch or os/arch/variant format.", platform))
			}
		}
	}
}

// sysCtxForRef returns a SystemContext with auth credentials for the given image reference's registry.
func (r *ImageCopyResource) sysCtxForRef(imageRef string) *itypes.SystemContext {
	if r.config == nil || r.config.AuthMap == nil {
		return nil
	}
	host := registryHost(imageRef)
	rc, ok := r.config.AuthMap[host]
	if !ok {
		return nil
	}
	sysCtx := &itypes.SystemContext{}
	if rc.Auth != nil {
		sysCtx.DockerAuthConfig = rc.Auth
	}
	if rc.InsecureSkipTLSVerify {
		sysCtx.DockerInsecureSkipTLSVerify = itypes.OptionalBoolTrue
	}
	return sysCtx
}

// registryHost extracts the registry hostname from a docker image reference.
func registryHost(imageRef string) string {
	imageRef = normalizeDockerRef(imageRef)
	const transport = "docker://"
	if !strings.HasPrefix(imageRef, transport) {
		return ""
	}
	ref, err := reference.ParseNormalizedNamed(strings.TrimPrefix(imageRef, transport))
	if err != nil {
		return ""
	}
	return reference.Domain(ref)
}

type copyResult struct {
	SourceDigest      string
	DestinationDigest string
	ArchDigests       map[string]string
}

func performCopy(ctx context.Context, r *ImageCopyResource, data *ImageCopyResourceModel) (*copyResult, error) {
	srcStr := normalizeDockerRef(data.Source.ValueString())
	destStr := normalizeDockerRef(data.Destination.ValueString())

	srcRef, err := alltransports.ParseImageName(srcStr)
	if err != nil {
		return nil, fmt.Errorf("invalid source: %w", err)
	}
	destRef, err := alltransports.ParseImageName(destStr)
	if err != nil {
		return nil, fmt.Errorf("invalid destination: %w", err)
	}

	policy, err := buildPolicyContext(r.config)
	if err != nil {
		return nil, fmt.Errorf("failed to create policy context: %w", err)
	}
	defer func() {
		if err := policy.Destroy(); err != nil {
			tflog.Warn(ctx, "failed to destroy policy context", map[string]any{"error": err.Error()})
		}
	}()

	srcSysCtx := r.sysCtxForRef(srcStr)
	destSysCtx := r.sysCtxForRef(destStr)

	// Read arch_tags and strict flags.
	var archTags map[string]string
	hasArchTags := !data.ArchTags.IsNull() && !data.ArchTags.IsUnknown()
	if hasArchTags {
		archTags = make(map[string]string)
		if diags := data.ArchTags.ElementsAs(ctx, &archTags, false); diags.HasError() {
			return nil, fmt.Errorf("failed to read arch_tags")
		}
	}
	strictArchTags := data.StrictArchTags.ValueBool()
	strictManifest := data.StrictManifest.ValueBool()

	result := &copyResult{
		ArchDigests: make(map[string]string),
	}

	// Fetch source manifest to determine if it's a manifest list and to get the source digest.
	imgSrc, err := srcRef.NewImageSource(ctx, srcSysCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to open image source: %w", err)
	}
	defer func() {
		if err := imgSrc.Close(); err != nil {
			tflog.Warn(ctx, "failed to close image source", map[string]any{"error": err.Error()})
		}
	}()

	manifestBytes, mimeType, err := imgSrc.GetManifest(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get manifest: %w", err)
	}

	srcDigest, err := manifest.Digest(manifestBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to compute source digest: %w", err)
	}
	result.SourceDigest = srcDigest.String()

	isList := isManifestList(mimeType)

	if !isList {
		// Single image.
		if hasArchTags {
			if strictManifest {
				return nil, fmt.Errorf("source image is not a manifest list but arch_tags is specified and strict_manifest is true")
			}
			tflog.Info(ctx, "Source image is not a manifest list; ignoring arch_tags")
		}

		opts := &copy.Options{SourceCtx: srcSysCtx, DestinationCtx: destSysCtx}
		destManifest, err := copy.Image(ctx, policy, destRef, srcRef, opts)
		if err != nil {
			return nil, fmt.Errorf("image copy failed: %w", err)
		}
		result.DestinationDigest = digestFromManifestBytes(destManifest)

		tflog.Info(ctx, "Single image copied successfully", map[string]any{
			"source":      data.Source.ValueString(),
			"destination": data.Destination.ValueString(),
		})
		return result, nil
	}

	// Source is a manifest list.
	var ml genericManifestList
	if err := json.Unmarshal(manifestBytes, &ml); err != nil {
		return nil, fmt.Errorf("failed to parse manifest list: %w", err)
	}

	if !hasArchTags {
		// No arch_tags → copy full manifest list as-is.
		opts := &copy.Options{
			ImageListSelection: copy.CopyAllImages,
			SourceCtx:          srcSysCtx,
			DestinationCtx:     destSysCtx,
		}
		destManifest, err := copy.Image(ctx, policy, destRef, srcRef, opts)
		if err != nil {
			return nil, fmt.Errorf("image copy failed: %w", err)
		}
		result.DestinationDigest = digestFromManifestBytes(destManifest)

		tflog.Info(ctx, "Full manifest list copied successfully", map[string]any{
			"source":      data.Source.ValueString(),
			"destination": data.Destination.ValueString(),
		})
		return result, nil
	}

	// arch_tags specified → copy filtered manifest list.
	// First, resolve each requested platform to its digest.
	type resolvedArch struct {
		platformKey string
		tag         string
		digest      string
		entry       manifestListEntry
	}
	var resolved []resolvedArch

	for platformKey, tag := range archTags {
		wantOS, wantArch, wantVariant := parsePlatform(platformKey)

		var matched *manifestListEntry
		for i := range ml.Manifests {
			entry := &ml.Manifests[i]
			if entry.Platform.OS == "" || entry.Platform.Architecture == "" {
				continue
			}
			if entry.Platform.OS != wantOS || entry.Platform.Architecture != wantArch {
				continue
			}
			if wantVariant != "" && entry.Platform.Variant != wantVariant {
				continue
			}
			matched = entry
			break
		}

		if matched == nil {
			if strictArchTags {
				return nil, fmt.Errorf("platform %q not found in source manifest list and strict_arch_tags is true", platformKey)
			}
			tflog.Warn(ctx, "No manifest entry found for platform; skipping", map[string]any{
				"platform": platformKey,
			})
			continue
		}

		resolved = append(resolved, resolvedArch{
			platformKey: platformKey,
			tag:         tag,
			digest:      matched.Digest,
			entry:       *matched,
		})
	}

	// Copy each per-arch image to its arch tag.
	for _, ra := range resolved {
		archSrcStr, err := buildDigestRef(srcStr, ra.digest)
		if err != nil {
			return nil, fmt.Errorf("failed to build source ref for platform %q: %w", ra.platformKey, err)
		}
		archDestStr, err := buildTaggedRef(destStr, ra.tag)
		if err != nil {
			return nil, fmt.Errorf("failed to build dest ref for platform %q: %w", ra.platformKey, err)
		}

		archSrcRef, err := alltransports.ParseImageName(archSrcStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse arch source ref for platform %q: %w", ra.platformKey, err)
		}
		archDestRef, err := alltransports.ParseImageName(archDestStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse arch dest ref for platform %q: %w", ra.platformKey, err)
		}

		opts := &copy.Options{SourceCtx: srcSysCtx, DestinationCtx: destSysCtx}
		if _, err = copy.Image(ctx, policy, archDestRef, archSrcRef, opts); err != nil {
			return nil, fmt.Errorf("failed to copy image for platform %q: %w", ra.platformKey, err)
		}

		result.ArchDigests[ra.platformKey] = ra.digest

		tflog.Info(ctx, "Copied per-arch image", map[string]any{
			"platform":    ra.platformKey,
			"source":      archSrcStr,
			"destination": archDestStr,
		})
	}

	// Build and push a filtered manifest list containing only the resolved platforms.
	filteredML := genericManifestList{
		SchemaVersion: ml.SchemaVersion,
		MediaType:     ml.MediaType,
	}
	for _, ra := range resolved {
		filteredML.Manifests = append(filteredML.Manifests, ra.entry)
	}
	filteredBytes, err := json.Marshal(filteredML)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal filtered manifest list: %w", err)
	}

	// Push the filtered manifest list to the destination tag.
	destImgDest, err := destRef.NewImageDestination(ctx, destSysCtx)
	if err != nil {
		return nil, fmt.Errorf("failed to open destination for filtered manifest list: %w", err)
	}
	defer func() {
		if err := destImgDest.Close(); err != nil {
			tflog.Warn(ctx, "failed to close image destination", map[string]any{"error": err.Error()})
		}
	}()

	if err := destImgDest.PutManifest(ctx, filteredBytes, nil); err != nil {
		return nil, fmt.Errorf("failed to push filtered manifest list: %w", err)
	}
	if err := destImgDest.Commit(ctx, nil); err != nil {
		return nil, fmt.Errorf("failed to commit filtered manifest list: %w", err)
	}

	result.DestinationDigest = digestFromManifestBytes(filteredBytes)

	tflog.Info(ctx, "Filtered manifest list copied successfully", map[string]any{
		"source":      data.Source.ValueString(),
		"destination": data.Destination.ValueString(),
		"platforms":   len(resolved),
	})

	return result, nil
}

func (r *ImageCopyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data ImageCopyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	result, err := performCopy(ctx, r, &data)
	if err != nil {
		resp.Diagnostics.AddError("Image Copy Error", err.Error())
		return
	}

	data.Id = data.Destination
	data.SourceDigest = types.StringValue(result.SourceDigest)
	data.DestinationDigest = types.StringValue(result.DestinationDigest)
	archDigests, diags := types.MapValueFrom(ctx, types.StringType, result.ArchDigests)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.ArchDigests = archDigests

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *ImageCopyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data ImageCopyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// No-op: state is treated as authoritative.
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (r *ImageCopyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Unreachable: all input attributes have RequiresReplace, so Terraform will always destroy+recreate.
	resp.Diagnostics.AddError("Unexpected Update", "Update should never be called; all attributes require replacement.")
}

func (r *ImageCopyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	// No-op: removes from state only, does not delete from registry.
}

// isManifestList returns true for OCI image indexes and Docker manifest lists.
func isManifestList(mimeType string) bool {
	switch mimeType {
	case ocispec.MediaTypeImageIndex,
		"application/vnd.docker.distribution.manifest.list.v2+json":
		return true
	}
	return false
}

// parsePlatform splits "linux/amd64" or "linux/arm/v7" into (os, arch, variant).
func parsePlatform(platform string) (os, arch, variant string) {
	parts := strings.SplitN(platform, "/", 3)
	switch len(parts) {
	case 1:
		return parts[0], "", ""
	case 2:
		return parts[0], parts[1], ""
	default:
		return parts[0], parts[1], parts[2]
	}
}

// normalizeDockerRef prepends "docker://" if the reference has no recognized transport prefix.
func normalizeDockerRef(ref string) string {
	if _, err := alltransports.ParseImageName(ref); err == nil {
		return ref
	}
	return "docker://" + ref
}

// buildDigestRef constructs a docker:// reference addressing a specific digest, stripping any tag.
func buildDigestRef(imageRef, dgst string) (string, error) {
	const transport = "docker://"
	imageRef = normalizeDockerRef(imageRef)
	if !strings.HasPrefix(imageRef, transport) {
		return "", fmt.Errorf("per-arch copy only supports docker:// transport, got: %s", imageRef)
	}
	ref, err := reference.ParseNormalizedNamed(strings.TrimPrefix(imageRef, transport))
	if err != nil {
		return "", fmt.Errorf("failed to parse reference: %w", err)
	}
	return transport + reference.TrimNamed(ref).Name() + "@" + dgst, nil
}

// buildTaggedRef constructs a docker:// reference using the destination repo and a specific tag.
func buildTaggedRef(imageRef, tag string) (string, error) {
	const transport = "docker://"
	imageRef = normalizeDockerRef(imageRef)
	if !strings.HasPrefix(imageRef, transport) {
		return "", fmt.Errorf("per-arch copy only supports docker:// transport, got: %s", imageRef)
	}
	ref, err := reference.ParseNormalizedNamed(strings.TrimPrefix(imageRef, transport))
	if err != nil {
		return "", fmt.Errorf("failed to parse reference: %w", err)
	}
	newRef, err := reference.WithTag(reference.TrimNamed(ref), tag)
	if err != nil {
		return "", fmt.Errorf("failed to build tagged reference: %w", err)
	}
	return transport + newRef.String(), nil
}

// digestFromManifestBytes computes a sha256 digest from raw manifest bytes.
func digestFromManifestBytes(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return digest.FromBytes(b).String()
}

// buildPolicyContext creates a signature PolicyContext from the provider configuration.
func buildPolicyContext(cfg *ProviderConfig) (*signature.PolicyContext, error) {
	method := SignatureMethodInsecure
	if cfg != nil {
		method = cfg.SignatureMethod
	}

	var reqs signature.PolicyRequirements
	switch method {
	case SignatureMethodInsecure, "":
		reqs = signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}
	case SignatureMethodReject:
		reqs = signature.PolicyRequirements{signature.NewPRReject()}
	case SignatureMethodSignedBy:
		pr, err := signature.NewPRSignedByKeyPath(signature.SBKeyTypeGPGKeys, cfg.SignatureKeyPath, signature.NewPRMMatchRepoDigestOrExact())
		if err != nil {
			return nil, fmt.Errorf("failed to create signedBy policy: %w", err)
		}
		reqs = signature.PolicyRequirements{pr}
	case SignatureMethodSigstoreSigned:
		pr, err := signature.NewPRSigstoreSignedKeyPath(cfg.SignatureKeyPath, signature.NewPRMMatchRepoDigestOrExact())
		if err != nil {
			return nil, fmt.Errorf("failed to create sigstoreSigned policy: %w", err)
		}
		reqs = signature.PolicyRequirements{pr}
	default:
		return nil, fmt.Errorf("unsupported signature method: %s", method)
	}

	return signature.NewPolicyContext(&signature.Policy{Default: reqs})
}

// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	ggcrTypes "github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	itypes "go.podman.io/image/v5/types"
)

// --- unit tests for helper functions ---

func TestParsePlatform(t *testing.T) {
	cases := []struct {
		in      string
		os      string
		arch    string
		variant string
	}{
		{"linux/amd64", "linux", "amd64", ""},
		{"linux/arm64", "linux", "arm64", ""},
		{"linux/arm/v7", "linux", "arm", "v7"},
		{"linux", "linux", "", ""},
	}
	for _, c := range cases {
		os, arch, variant := parsePlatform(c.in)
		if os != c.os || arch != c.arch || variant != c.variant {
			t.Errorf("parsePlatform(%q) = %q/%q/%q, want %q/%q/%q",
				c.in, os, arch, variant, c.os, c.arch, c.variant)
		}
	}
}

func TestNormalizeDockerRef(t *testing.T) {
	cases := []struct{ in, want string }{
		{"hello-world", "docker://hello-world"},
		{"alpine:3.21", "docker://alpine:3.21"},
		{"myregistry.com/myimage:tag", "docker://myregistry.com/myimage:tag"},
		{"localhost:5000/myimage:tag", "docker://localhost:5000/myimage:tag"},
		{"docker://already/prefixed:tag", "docker://already/prefixed:tag"},
		{"oci-archive:/tmp/image.tar", "oci-archive:/tmp/image.tar"},
	}
	for _, c := range cases {
		if got := normalizeDockerRef(c.in); got != c.want {
			t.Errorf("normalizeDockerRef(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildDigestRef(t *testing.T) {
	cases := []struct {
		ref    string
		digest string
		want   string
	}{
		{
			"docker://myregistry.example.com/myimage:latest",
			"sha256:abc123",
			"docker://myregistry.example.com/myimage@sha256:abc123",
		},
		{
			"docker://localhost:5000/myimage:v1.2.3",
			"sha256:deadbeef",
			"docker://localhost:5000/myimage@sha256:deadbeef",
		},
		{
			"myregistry.example.com/myimage:latest",
			"sha256:abc123",
			"docker://myregistry.example.com/myimage@sha256:abc123",
		},
		{
			"localhost:5000/myimage",
			"sha256:deadbeef",
			"docker://localhost:5000/myimage@sha256:deadbeef",
		},
		{
			"hello-world",
			"sha256:abc123",
			"docker://docker.io/library/hello-world@sha256:abc123",
		},
	}
	for _, c := range cases {
		got, err := buildDigestRef(c.ref, c.digest)
		if err != nil {
			t.Errorf("buildDigestRef(%q, %q) error: %v", c.ref, c.digest, err)
			continue
		}
		if got != c.want {
			t.Errorf("buildDigestRef(%q, %q) = %q, want %q", c.ref, c.digest, got, c.want)
		}
	}
}

func TestBuildDigestRef_NonDocker(t *testing.T) {
	_, err := buildDigestRef("oci-archive:/tmp/image.tar", "sha256:abc")
	if err == nil {
		t.Error("expected error for non-docker transport")
	}
}

func TestBuildTaggedRef(t *testing.T) {
	cases := []struct {
		ref  string
		tag  string
		want string
	}{
		{
			"docker://myregistry.example.com/myimage:latest",
			"3.21-linux-amd64",
			"docker://myregistry.example.com/myimage:3.21-linux-amd64",
		},
		{
			"docker://localhost:5000/myimage:v1.2.3",
			"3.21-linux-arm64",
			"docker://localhost:5000/myimage:3.21-linux-arm64",
		},
		{
			"myregistry.example.com/myimage:v1.2.3",
			"3.21-linux-amd64",
			"docker://myregistry.example.com/myimage:3.21-linux-amd64",
		},
		{
			"localhost:5000/myimage",
			"3.21-linux-arm64",
			"docker://localhost:5000/myimage:3.21-linux-arm64",
		},
		{
			"hello-world",
			"3.21-linux-amd64",
			"docker://docker.io/library/hello-world:3.21-linux-amd64",
		},
	}
	for _, c := range cases {
		got, err := buildTaggedRef(c.ref, c.tag)
		if err != nil {
			t.Errorf("buildTaggedRef(%q, %q) error: %v", c.ref, c.tag, err)
			continue
		}
		if got != c.want {
			t.Errorf("buildTaggedRef(%q, %q) = %q, want %q", c.ref, c.tag, got, c.want)
		}
	}
}

func TestIsManifestList(t *testing.T) {
	if !isManifestList("application/vnd.docker.distribution.manifest.list.v2+json") {
		t.Error("expected docker manifest list to be detected")
	}
	if !isManifestList("application/vnd.oci.image.index.v1+json") {
		t.Error("expected OCI image index to be detected")
	}
	if isManifestList("application/vnd.docker.distribution.manifest.v2+json") {
		t.Error("single-arch manifest should not be detected as list")
	}
}

func TestRegistryHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"docker://myregistry.com/repo:tag", "myregistry.com"},
		{"myregistry.com/repo:tag", "myregistry.com"},
		{"alpine:latest", "docker.io"},
		{"docker://docker.io/library/alpine:latest", "docker.io"},
	}
	for _, c := range cases {
		if got := registryHost(c.in); got != c.want {
			t.Errorf("registryHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- integration test using in-process registries ---

func startRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func pushMultiArchIndex(t *testing.T, host, repo, tag string) map[string]string {
	t.Helper()

	type platform struct{ os, arch string }
	platforms := []platform{{"linux", "amd64"}, {"linux", "arm64"}}

	digests := make(map[string]string)
	var addenda []mutate.IndexAddendum
	for _, p := range platforms {
		img, err := random.Image(512, 1)
		if err != nil {
			t.Fatalf("random.Image: %v", err)
		}
		d, err := img.Digest()
		if err != nil {
			t.Fatalf("img.Digest: %v", err)
		}
		digests[p.os+"/"+p.arch] = d.String()
		addenda = append(addenda, mutate.IndexAddendum{
			Add: img,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{OS: p.os, Architecture: p.arch},
			},
		})
	}

	idx := mutate.AppendManifests(
		mutate.IndexMediaType(empty.Index, ggcrTypes.OCIImageIndex),
		addenda...,
	)

	ref, err := name.ParseReference(fmt.Sprintf("%s/%s:%s", host, repo, tag), name.Insecure)
	if err != nil {
		t.Fatalf("name.ParseReference: %v", err)
	}
	if err := remote.WriteIndex(ref, idx, remote.WithTransport(remote.DefaultTransport)); err != nil {
		t.Fatalf("remote.WriteIndex: %v", err)
	}
	return digests
}

func newTestResource(hosts ...string) *ImageCopyResource {
	authMap := make(RegistryAuthMap)
	for _, h := range hosts {
		authMap[h] = &RegistryConfig{InsecureSkipTLSVerify: true}
	}
	return &ImageCopyResource{
		config: &ProviderConfig{
			AuthMap:         authMap,
			SignatureMethod: SignatureMethodInsecure,
		},
	}
}

func TestCopyImage(t *testing.T) {
	t.Run("single arch", func(t *testing.T) {
		srcHost := startRegistry(t)
		dstHost := startRegistry(t)

		img, err := random.Image(512, 1)
		if err != nil {
			t.Fatalf("random.Image: %v", err)
		}
		ref, err := name.ParseReference(fmt.Sprintf("%s/test/image:latest", srcHost), name.Insecure)
		if err != nil {
			t.Fatalf("name.ParseReference: %v", err)
		}
		if err := remote.Write(ref, img); err != nil {
			t.Fatalf("remote.Write: %v", err)
		}

		data := &ImageCopyResourceModel{
			Source:         types.StringValue(fmt.Sprintf("%s/test/image:latest", srcHost)),
			Destination:    types.StringValue(fmt.Sprintf("%s/test/image:latest", dstHost)),
			ArchTags:       types.MapNull(types.StringType),
			StrictArchTags: types.BoolValue(false),
			StrictManifest: types.BoolValue(false),
		}
		result, err := performCopy(t.Context(), newTestResource(srcHost, dstHost), data)
		if err != nil {
			t.Fatalf("performCopy: %v", err)
		}
		if result.SourceDigest == "" {
			t.Error("expected non-empty source_digest")
		}
		if result.DestinationDigest == "" {
			t.Error("expected non-empty destination_digest")
		}

		dstRef, err := name.ParseReference(fmt.Sprintf("%s/test/image:latest", dstHost), name.Insecure)
		if err != nil {
			t.Fatalf("name.ParseReference: %v", err)
		}
		if _, err := remote.Head(dstRef); err != nil {
			t.Errorf("expected image at destination, got: %v", err)
		}
	})

	t.Run("multi arch no arch_tags", func(t *testing.T) {
		srcHost := startRegistry(t)
		dstHost := startRegistry(t)

		_ = pushMultiArchIndex(t, srcHost, "test/image", "latest")

		data := &ImageCopyResourceModel{
			Source:         types.StringValue(fmt.Sprintf("%s/test/image:latest", srcHost)),
			Destination:    types.StringValue(fmt.Sprintf("%s/test/image:latest", dstHost)),
			ArchTags:       types.MapNull(types.StringType),
			StrictArchTags: types.BoolValue(false),
			StrictManifest: types.BoolValue(false),
		}
		result, err := performCopy(t.Context(), newTestResource(srcHost, dstHost), data)
		if err != nil {
			t.Fatalf("performCopy: %v", err)
		}
		if result.SourceDigest == "" {
			t.Error("expected non-empty source_digest")
		}
		if result.DestinationDigest == "" {
			t.Error("expected non-empty destination_digest")
		}

		dstRef, err := name.ParseReference(fmt.Sprintf("%s/test/image:latest", dstHost), name.Insecure)
		if err != nil {
			t.Fatalf("name.ParseReference: %v", err)
		}
		idx, err := remote.Index(dstRef)
		if err != nil {
			t.Fatalf("expected manifest list at destination, got: %v", err)
		}
		manifest, err := idx.IndexManifest()
		if err != nil {
			t.Fatalf("IndexManifest: %v", err)
		}
		if got := len(manifest.Manifests); got != 2 {
			t.Errorf("expected 2 platform manifests, got %d", got)
		}
	})

	t.Run("single arch with arch_tags and strict_manifest", func(t *testing.T) {
		srcHost := startRegistry(t)
		dstHost := startRegistry(t)

		img, err := random.Image(512, 1)
		if err != nil {
			t.Fatalf("random.Image: %v", err)
		}
		ref, err := name.ParseReference(fmt.Sprintf("%s/test/image:latest", srcHost), name.Insecure)
		if err != nil {
			t.Fatalf("name.ParseReference: %v", err)
		}
		if err := remote.Write(ref, img); err != nil {
			t.Fatalf("remote.Write: %v", err)
		}

		archTags, diags := types.MapValue(types.StringType, map[string]attr.Value{
			"linux/amd64": types.StringValue("latest-amd64"),
		})
		if diags.HasError() {
			t.Fatalf("building arch_tags: %v", diags)
		}

		data := &ImageCopyResourceModel{
			Source:         types.StringValue(fmt.Sprintf("%s/test/image:latest", srcHost)),
			Destination:    types.StringValue(fmt.Sprintf("%s/test/image:latest", dstHost)),
			ArchTags:       archTags,
			StrictArchTags: types.BoolValue(false),
			StrictManifest: types.BoolValue(true),
		}
		_, err = performCopy(t.Context(), newTestResource(srcHost, dstHost), data)
		if err == nil {
			t.Fatal("expected error when strict_manifest=true and source is not a manifest list")
		}
		if !strings.Contains(err.Error(), "strict_manifest") {
			t.Errorf("expected error mentioning strict_manifest, got: %v", err)
		}
	})

	t.Run("single arch with arch_tags no strict_manifest copies image", func(t *testing.T) {
		srcHost := startRegistry(t)
		dstHost := startRegistry(t)

		img, err := random.Image(512, 1)
		if err != nil {
			t.Fatalf("random.Image: %v", err)
		}
		ref, err := name.ParseReference(fmt.Sprintf("%s/test/image:latest", srcHost), name.Insecure)
		if err != nil {
			t.Fatalf("name.ParseReference: %v", err)
		}
		if err := remote.Write(ref, img); err != nil {
			t.Fatalf("remote.Write: %v", err)
		}

		archTags, diags := types.MapValue(types.StringType, map[string]attr.Value{
			"linux/amd64": types.StringValue("latest-amd64"),
		})
		if diags.HasError() {
			t.Fatalf("building arch_tags: %v", diags)
		}

		data := &ImageCopyResourceModel{
			Source:         types.StringValue(fmt.Sprintf("%s/test/image:latest", srcHost)),
			Destination:    types.StringValue(fmt.Sprintf("%s/test/image:latest", dstHost)),
			ArchTags:       archTags,
			StrictArchTags: types.BoolValue(false),
			StrictManifest: types.BoolValue(false),
		}
		result, err := performCopy(t.Context(), newTestResource(srcHost, dstHost), data)
		if err != nil {
			t.Fatalf("performCopy: %v", err)
		}
		if result.DestinationDigest == "" {
			t.Error("expected non-empty destination_digest")
		}

		dstRef, err := name.ParseReference(fmt.Sprintf("%s/test/image:latest", dstHost), name.Insecure)
		if err != nil {
			t.Fatalf("name.ParseReference: %v", err)
		}
		if _, err := remote.Head(dstRef); err != nil {
			t.Errorf("expected image at destination, got: %v", err)
		}
	})
}

func TestCopyPerArchImages(t *testing.T) {
	srcHost := startRegistry(t)
	dstHost := startRegistry(t)

	_ = pushMultiArchIndex(t, srcHost, "test/image", "latest")

	archTags, diags := types.MapValue(types.StringType, map[string]attr.Value{
		"linux/amd64": types.StringValue("3.21-linux-amd64"),
		"linux/arm64": types.StringValue("3.21-linux-arm64"),
	})
	if diags.HasError() {
		t.Fatalf("building arch_tags: %v", diags)
	}

	data := &ImageCopyResourceModel{
		Source:         types.StringValue(fmt.Sprintf("%s/test/image:latest", srcHost)),
		Destination:    types.StringValue(fmt.Sprintf("%s/test/image:latest", dstHost)),
		ArchTags:       archTags,
		StrictArchTags: types.BoolValue(false),
		StrictManifest: types.BoolValue(false),
	}
	result, err := performCopy(t.Context(), newTestResource(srcHost, dstHost), data)
	if err != nil {
		t.Fatalf("performCopy: %v", err)
	}

	if len(result.ArchDigests) != 2 {
		t.Errorf("expected 2 arch digests, got %d", len(result.ArchDigests))
	}

	for _, tag := range []string{"3.21-linux-amd64", "3.21-linux-arm64"} {
		ref, err := name.ParseReference(fmt.Sprintf("%s/test/image:%s", dstHost, tag), name.Insecure)
		if err != nil {
			t.Fatalf("parse ref for %s: %v", tag, err)
		}
		if _, err := remote.Head(ref); err != nil {
			t.Errorf("expected tag %q to exist in destination registry, got: %v", tag, err)
		}
	}
}

func TestStrictArchTags(t *testing.T) {
	srcHost := startRegistry(t)
	dstHost := startRegistry(t)

	_ = pushMultiArchIndex(t, srcHost, "test/image", "latest")

	archTags, diags := types.MapValue(types.StringType, map[string]attr.Value{
		"linux/amd64": types.StringValue("3.21-linux-amd64"),
		"linux/s390x": types.StringValue("3.21-linux-s390x"), // does not exist
	})
	if diags.HasError() {
		t.Fatalf("building arch_tags: %v", diags)
	}

	t.Run("strict fails on missing platform", func(t *testing.T) {
		data := &ImageCopyResourceModel{
			Source:         types.StringValue(fmt.Sprintf("%s/test/image:latest", srcHost)),
			Destination:    types.StringValue(fmt.Sprintf("%s/test/image:latest", dstHost)),
			ArchTags:       archTags,
			StrictArchTags: types.BoolValue(true),
			StrictManifest: types.BoolValue(false),
		}
		_, err := performCopy(t.Context(), newTestResource(srcHost, dstHost), data)
		if err == nil {
			t.Fatal("expected error when strict_arch_tags=true and platform is missing")
		}
		if !strings.Contains(err.Error(), "strict_arch_tags") {
			t.Errorf("expected error mentioning strict_arch_tags, got: %v", err)
		}
	})

	t.Run("non-strict skips missing platform", func(t *testing.T) {
		data := &ImageCopyResourceModel{
			Source:         types.StringValue(fmt.Sprintf("%s/test/image:latest", srcHost)),
			Destination:    types.StringValue(fmt.Sprintf("%s/test/image:latest", dstHost)),
			ArchTags:       archTags,
			StrictArchTags: types.BoolValue(false),
			StrictManifest: types.BoolValue(false),
		}
		result, err := performCopy(t.Context(), newTestResource(srcHost, dstHost), data)
		if err != nil {
			t.Fatalf("performCopy: %v", err)
		}
		// Only linux/amd64 should be in arch_digests, s390x was skipped.
		if len(result.ArchDigests) != 1 {
			t.Errorf("expected 1 arch digest (s390x skipped), got %d", len(result.ArchDigests))
		}
		if _, ok := result.ArchDigests["linux/amd64"]; !ok {
			t.Error("expected linux/amd64 in arch_digests")
		}
	})
}

func pushMultiArchIndexWithVariant(t *testing.T, host, repo, tag string) map[string]string {
	t.Helper()

	type platform struct{ os, arch, variant string }
	platforms := []platform{
		{"linux", "amd64", ""},
		{"linux", "arm64", "v8"},
		{"linux", "arm", "v7"},
	}

	digests := make(map[string]string)
	var addenda []mutate.IndexAddendum
	for _, p := range platforms {
		img, err := random.Image(512, 1)
		if err != nil {
			t.Fatalf("random.Image: %v", err)
		}
		d, err := img.Digest()
		if err != nil {
			t.Fatalf("img.Digest: %v", err)
		}
		key := p.os + "/" + p.arch
		if p.variant != "" {
			key += "/" + p.variant
		}
		digests[key] = d.String()
		plat := &v1.Platform{OS: p.os, Architecture: p.arch}
		if p.variant != "" {
			plat.Variant = p.variant
		}
		addenda = append(addenda, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: plat},
		})
	}

	idx := mutate.AppendManifests(
		mutate.IndexMediaType(empty.Index, ggcrTypes.OCIImageIndex),
		addenda...,
	)

	ref, err := name.ParseReference(fmt.Sprintf("%s/%s:%s", host, repo, tag), name.Insecure)
	if err != nil {
		t.Fatalf("name.ParseReference: %v", err)
	}
	if err := remote.WriteIndex(ref, idx, remote.WithTransport(remote.DefaultTransport)); err != nil {
		t.Fatalf("remote.WriteIndex: %v", err)
	}
	return digests
}

func TestCopyPerArchImages_variant(t *testing.T) {
	srcHost := startRegistry(t)
	dstHost := startRegistry(t)

	_ = pushMultiArchIndexWithVariant(t, srcHost, "test/image", "latest")

	archTags, diags := types.MapValue(types.StringType, map[string]attr.Value{
		"linux/arm64/v8": types.StringValue("latest-arm64-v8"),
		"linux/arm/v7":   types.StringValue("latest-arm-v7"),
	})
	if diags.HasError() {
		t.Fatalf("building arch_tags: %v", diags)
	}

	data := &ImageCopyResourceModel{
		Source:         types.StringValue(fmt.Sprintf("%s/test/image:latest", srcHost)),
		Destination:    types.StringValue(fmt.Sprintf("%s/test/image:latest", dstHost)),
		ArchTags:       archTags,
		StrictArchTags: types.BoolValue(true),
		StrictManifest: types.BoolValue(false),
	}
	result, err := performCopy(t.Context(), newTestResource(srcHost, dstHost), data)
	if err != nil {
		t.Fatalf("performCopy: %v", err)
	}

	if len(result.ArchDigests) != 2 {
		t.Errorf("expected 2 arch digests, got %d", len(result.ArchDigests))
	}
	if _, ok := result.ArchDigests["linux/arm64/v8"]; !ok {
		t.Error("expected linux/arm64/v8 in arch_digests")
	}
	if _, ok := result.ArchDigests["linux/arm/v7"]; !ok {
		t.Error("expected linux/arm/v7 in arch_digests")
	}

	for _, tag := range []string{"latest-arm64-v8", "latest-arm-v7"} {
		ref, err := name.ParseReference(fmt.Sprintf("%s/test/image:%s", dstHost, tag), name.Insecure)
		if err != nil {
			t.Fatalf("parse ref for %s: %v", tag, err)
		}
		if _, err := remote.Head(ref); err != nil {
			t.Errorf("expected tag %q to exist in destination registry, got: %v", tag, err)
		}
	}
}

func TestSysCtxForRef(t *testing.T) {
	t.Run("no config returns nil", func(t *testing.T) {
		r := &ImageCopyResource{}
		if got := r.sysCtxForRef("docker://example.com/repo:tag"); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("unmatched host returns nil", func(t *testing.T) {
		r := &ImageCopyResource{
			config: &ProviderConfig{
				AuthMap: RegistryAuthMap{
					"other.io": &RegistryConfig{},
				},
			},
		}
		if got := r.sysCtxForRef("docker://example.com/repo:tag"); got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("matched host with auth", func(t *testing.T) {
		r := &ImageCopyResource{
			config: &ProviderConfig{
				AuthMap: RegistryAuthMap{
					"example.com": &RegistryConfig{
						Auth: &itypes.DockerAuthConfig{
							Username: "user",
							Password: "pass",
						},
					},
				},
			},
		}
		got := r.sysCtxForRef("docker://example.com/repo:tag")
		if got == nil {
			t.Fatal("expected non-nil SystemContext")
		}
		if got.DockerAuthConfig == nil || got.DockerAuthConfig.Username != "user" || got.DockerAuthConfig.Password != "pass" {
			t.Errorf("unexpected auth config: %+v", got.DockerAuthConfig)
		}
	})

	t.Run("matched host with insecure", func(t *testing.T) {
		r := &ImageCopyResource{
			config: &ProviderConfig{
				AuthMap: RegistryAuthMap{
					"example.com": &RegistryConfig{
						InsecureSkipTLSVerify: true,
					},
				},
			},
		}
		got := r.sysCtxForRef("docker://example.com/repo:tag")
		if got == nil {
			t.Fatal("expected non-nil SystemContext")
		}
		if got.DockerInsecureSkipTLSVerify != itypes.OptionalBoolTrue {
			t.Error("expected DockerInsecureSkipTLSVerify to be true")
		}
	})
}

func TestBuildPolicyContext(t *testing.T) {
	t.Run("nil config defaults to insecure", func(t *testing.T) {
		pc, err := buildPolicyContext(nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = pc.Destroy() }()
	})

	t.Run("insecureAcceptAnything", func(t *testing.T) {
		pc, err := buildPolicyContext(&ProviderConfig{SignatureMethod: SignatureMethodInsecure})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = pc.Destroy() }()
	})

	t.Run("reject", func(t *testing.T) {
		pc, err := buildPolicyContext(&ProviderConfig{SignatureMethod: SignatureMethodReject})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = pc.Destroy() }()
	})

	t.Run("signedBy with key file", func(t *testing.T) {
		keyFile, err := os.CreateTemp(t.TempDir(), "key-*.gpg")
		if err != nil {
			t.Fatalf("creating temp file: %v", err)
		}
		keyFile.Close()

		pc, err := buildPolicyContext(&ProviderConfig{
			SignatureMethod:  SignatureMethodSignedBy,
			SignatureKeyPath: keyFile.Name(),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = pc.Destroy() }()
	})

	t.Run("sigstoreSigned with key file", func(t *testing.T) {
		keyFile, err := os.CreateTemp(t.TempDir(), "key-*.pub")
		if err != nil {
			t.Fatalf("creating temp file: %v", err)
		}
		keyFile.Close()

		pc, err := buildPolicyContext(&ProviderConfig{
			SignatureMethod:  SignatureMethodSigstoreSigned,
			SignatureKeyPath: keyFile.Name(),
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer func() { _ = pc.Destroy() }()
	})

	t.Run("unsupported method errors", func(t *testing.T) {
		_, err := buildPolicyContext(&ProviderConfig{SignatureMethod: "bogus"})
		if err == nil {
			t.Fatal("expected error for unsupported method")
		}
		if !strings.Contains(err.Error(), "unsupported signature method") {
			t.Errorf("unexpected error: %v", err)
		}
	})
}

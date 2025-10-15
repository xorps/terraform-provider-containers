// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func pushSingleImage(t *testing.T, host, repo, tag string) {
	t.Helper()
	img, err := random.Image(512, 1)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	ref, err := name.ParseReference(fmt.Sprintf("%s/%s:%s", host, repo, tag), name.Insecure)
	if err != nil {
		t.Fatalf("name.ParseReference: %v", err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatalf("remote.Write: %v", err)
	}
}

func testAccProviderConfig(registryHosts ...string) string {
	cfg := `provider "containers" {` + "\n"
	for _, host := range registryHosts {
		cfg += fmt.Sprintf(`
  registry {
    hostname                = %q
    insecure_skip_tls_verify = true
  }
`, host)
	}
	cfg += "}\n"
	return cfg
}

func TestAccImageCopy_basic(t *testing.T) {
	srcHost := startRegistry(t)
	dstHost := startRegistry(t)
	pushSingleImage(t, srcHost, "test/image", "latest")

	config := testAccProviderConfig(srcHost, dstHost) + fmt.Sprintf(`
resource "containers_image_copy" "test" {
  source      = %q
  destination = %q
}
`, srcHost+"/test/image:latest", dstHost+"/test/image:latest")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("containers_image_copy.test", "source", srcHost+"/test/image:latest"),
					resource.TestCheckResourceAttr("containers_image_copy.test", "destination", dstHost+"/test/image:latest"),
					resource.TestCheckResourceAttrSet("containers_image_copy.test", "source_digest"),
					resource.TestCheckResourceAttrSet("containers_image_copy.test", "destination_digest"),
					resource.TestCheckResourceAttr("containers_image_copy.test", "id", dstHost+"/test/image:latest"),
				),
			},
		},
	})
}

func TestAccImageCopy_multiArch(t *testing.T) {
	srcHost := startRegistry(t)
	dstHost := startRegistry(t)
	pushMultiArchIndex(t, srcHost, "test/image", "latest")

	config := testAccProviderConfig(srcHost, dstHost) + fmt.Sprintf(`
resource "containers_image_copy" "test" {
  source      = %q
  destination = %q
}
`, srcHost+"/test/image:latest", dstHost+"/test/image:latest")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrSet("containers_image_copy.test", "source_digest"),
					resource.TestCheckResourceAttrSet("containers_image_copy.test", "destination_digest"),
				),
			},
		},
	})
}

func TestAccImageCopy_multiArchWithArchTags(t *testing.T) {
	srcHost := startRegistry(t)
	dstHost := startRegistry(t)
	digests := pushMultiArchIndex(t, srcHost, "test/image", "latest")

	config := testAccProviderConfig(srcHost, dstHost) + fmt.Sprintf(`
resource "containers_image_copy" "test" {
  source      = %q
  destination = %q
  arch_tags = {
    "linux/amd64" = "latest-amd64"
    "linux/arm64" = "latest-arm64"
  }
}
`, srcHost+"/test/image:latest", dstHost+"/test/image:latest")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttrSet("containers_image_copy.test", "source_digest"),
					resource.TestCheckResourceAttrSet("containers_image_copy.test", "destination_digest"),
					resource.TestCheckResourceAttr("containers_image_copy.test", "arch_digests.linux/amd64", digests["linux/amd64"]),
					resource.TestCheckResourceAttr("containers_image_copy.test", "arch_digests.linux/arm64", digests["linux/arm64"]),
				),
			},
		},
	})
}

func TestAccImageCopy_forceNew(t *testing.T) {
	srcHost := startRegistry(t)
	dstHost := startRegistry(t)
	pushSingleImage(t, srcHost, "test/image-a", "v1")
	pushSingleImage(t, srcHost, "test/image-b", "v1")

	configA := testAccProviderConfig(srcHost, dstHost) + fmt.Sprintf(`
resource "containers_image_copy" "test" {
  source      = %q
  destination = %q
}
`, srcHost+"/test/image-a:v1", dstHost+"/test/dest:v1")

	configB := testAccProviderConfig(srcHost, dstHost) + fmt.Sprintf(`
resource "containers_image_copy" "test" {
  source      = %q
  destination = %q
}
`, srcHost+"/test/image-b:v1", dstHost+"/test/dest:v1")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: configA,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("containers_image_copy.test", "source", srcHost+"/test/image-a:v1"),
					resource.TestCheckResourceAttrSet("containers_image_copy.test", "source_digest"),
				),
			},
			{
				Config: configB,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("containers_image_copy.test", "source", srcHost+"/test/image-b:v1"),
					resource.TestCheckResourceAttrSet("containers_image_copy.test", "source_digest"),
				),
			},
		},
	})
}

func TestAccImageCopy_strictManifestError(t *testing.T) {
	srcHost := startRegistry(t)
	dstHost := startRegistry(t)
	pushSingleImage(t, srcHost, "test/image", "latest")

	config := testAccProviderConfig(srcHost, dstHost) + fmt.Sprintf(`
resource "containers_image_copy" "test" {
  source          = %q
  destination     = %q
  strict_manifest = true
  arch_tags = {
    "linux/amd64" = "latest-amd64"
  }
}
`, srcHost+"/test/image:latest", dstHost+"/test/image:latest")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexp.MustCompile(`strict_manifest`),
			},
		},
	})
}

func TestAccImageCopy_strictArchTagsError(t *testing.T) {
	srcHost := startRegistry(t)
	dstHost := startRegistry(t)
	pushMultiArchIndex(t, srcHost, "test/multiarch", "v1")

	config := testAccProviderConfig(srcHost, dstHost) + fmt.Sprintf(`
resource "containers_image_copy" "test" {
  source           = %q
  destination      = %q
  strict_arch_tags = true
  arch_tags = {
    "linux/s390x" = "latest-s390x"
  }
}
`, srcHost+"/test/multiarch:v1", dstHost+"/test/multiarch:v1")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexp.MustCompile(`strict_arch_tags`),
			},
		},
	})
}

func TestAccImageCopy_validateEmptyTagValue(t *testing.T) {
	config := testAccProviderConfig() + `
resource "containers_image_copy" "test" {
  source      = "example.com/repo:tag"
  destination = "example.com/dest:tag"
  arch_tags = {
    "linux/amd64" = ""
  }
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexp.MustCompile(`must not be empty`),
			},
		},
	})
}

func TestAccImageCopy_validateInvalidPlatform(t *testing.T) {
	config := testAccProviderConfig() + `
resource "containers_image_copy" "test" {
  source      = "example.com/repo:tag"
  destination = "example.com/dest:tag"
  arch_tags = {
    "justlinux" = "latest-amd64"
  }
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexp.MustCompile(`os/arch`),
			},
		},
	})
}

func TestAccImageCopy_validateDuplicateRegistry(t *testing.T) {
	config := `
provider "containers" {
  registry {
    hostname = "example.com"
  }
  registry {
    hostname = "example.com"
  }
}

resource "containers_image_copy" "test" {
  source      = "example.com/repo:tag"
  destination = "example.com/dest:tag"
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexp.MustCompile(`Duplicate Registry`),
			},
		},
	})
}

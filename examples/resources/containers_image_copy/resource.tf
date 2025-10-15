# Copy a single image
resource "containers_image_copy" "basic" {
  source      = "ghcr.io/myorg/myapp:v1.0.0"
  destination = "docker.io/myorg/myapp:v1.0.0"
}

# Copy a multi-arch image with per-architecture tags
resource "containers_image_copy" "multi_arch" {
  source      = "ghcr.io/myorg/myapp:v1.0.0"
  destination = "docker.io/myorg/myapp:v1.0.0"

  arch_tags = {
    "linux/amd64" = "v1.0.0-amd64"
    "linux/arm64" = "v1.0.0-arm64"
  }

  strict_arch_tags = true
}

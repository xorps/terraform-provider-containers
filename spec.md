# Terraform Provider Image Copy Resource
A new terraform resource (`image_copy_resource`) that copies a container image from one registry to another.

## Provider Configuration

Registry authentication and signature policy are configured at the provider level and do not affect resource lifecycle.

### `registry` block (repeatable, optional)
- `hostname` - string, required. Registry hostname (e.g. `docker.io`, `ghcr.io`).
- `username` - string, optional. Username for registry authentication.
- `password` - string, optional, sensitive. Password or token for registry authentication.
- `insecure_skip_tls_verify` - bool, optional. Skip TLS certificate verification for this registry. Useful for registries with self-signed certificates or HTTP-only registries.

### `signature_policy` block (optional)
If omitted, all images are accepted without signature verification (`insecureAcceptAnything`).
- `method` - string, optional. Signature verification method. Supported values: `insecureAcceptAnything` (default), `reject`, `signedBy`, `sigstoreSigned`.
- `signature_key_path` - string, optional. Path to a public key file. GPG public key for `signedBy`, cosign public key for `sigstoreSigned`. Required when method is `signedBy` or `sigstoreSigned`.

## Resource: `image_copy`

The attributes it supports

Input (required, ForceNew):
- `source` - string, the source image reference (e.g. `registry/repo:tag`)
- `destination` - string, the destination image reference (e.g. `registry/repo:tag`)

Input (optional, ForceNew):
- `arch_tags` - map(string), takes an OCI platform string as key (e.g. `linux/amd64`, `linux/arm64/v8`) and the final remote tag as value.
- `strict_arch_tags` - bool, (default: false) if true, fail when a platform specified in `arch_tags` is not found in the source manifest list.
- `strict_manifest` - bool, (default: false) if true, fail when `arch_tags` is specified but the source image is not a manifest list.

Computed:
- `source_digest` - the digest of the source image at time of copy.
- `destination_digest` - the digest of the copied manifest at the destination.
- `arch_digests` - map(string), platform → digest for each copied arch image.

Behavior:
- **Create**: copies the image as described below.
- **Read/Refresh**: no-op, state is treated as authoritative. Drift (e.g. image deleted from destination outside Terraform) is not detected.
- **Update**: unreachable — all input attributes are ForceNew, so Terraform always destroys and recreates.
- **Delete**: no-op, removes the resource from Terraform state only. The image is not deleted from the registry.
- **Import**: not supported.

Copy behavior:
- If image is manifest list and no arch_tags specified → copy full manifest list as-is.
- If image is manifest list and arch_tags specified → copy filtered manifest list (only specified arches) to destination tag, pushing each arch image under its arch_tags value. If a specified arch is not found: fail if `strict_arch_tags = true`, skip otherwise.
- If image is a single image and no arch_tags specified → copy it.
- If image is a single image and arch_tags specified → fail if `strict_manifest = true`, ignore arch_tags and copy otherwise.

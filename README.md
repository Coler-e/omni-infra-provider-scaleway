# omni-infra-provider-scaleway

An [Omni](https://github.com/siderolabs/omni) infrastructure provider for [Scaleway](https://www.scaleway.com/).
It provisions Scaleway Instances on demand and registers them with Omni as Talos machines.

## Features

- Automatic provisioning and deprovisioning of Scaleway Instances.
- Multi-zone distribution with guaranteed even spread (sequential provisioning with count-based zone selection).
- Multi-region support: fr-par, nl-ams, pl-waw (all zones).
- Automatic architecture inference from Scaleway's server-types API (no manual `arch` required).
- Image lookup by name with arch filter — one machine class works across all zones.
- Private Network attachment per region (Scaleway VPCs span zones within a region).
- QEMU guest agent pre-installed in every schematic.
- `omni-request-id` tagging for traceability.

## Prerequisites

- A running [Omni](https://omni.siderolabs.com/) instance (self-hosted or SaaS).
- A Scaleway account with an API key (access key + secret key).
- A Talos OS image uploaded to Scaleway (one per zone). See [Image Setup](#image-setup).
- An Omni service account key for the provider.

## Image Setup

Upload the Talos disk image to Scaleway Object Storage and import it as a custom image in each zone you intend to use.
The image name must follow the pattern `<image_name>` (e.g. `talos-v1.9.0`).
The provider uses the Scaleway image's arch metadata, so no suffix is needed.

Download the Talos disk images from [factory.talos.dev](https://factory.talos.dev/) — select the Scaleway platform and your schematic, then download the `.qcow2` for each architecture you need.

Example using the Scaleway CLI (repeat for each zone and each arch):

```bash
# --- amd64 ---
# Upload to Object Storage
aws s3 cp talos-amd64.qcow2 s3://my-bucket/talos-amd64.qcow2 \
  --endpoint-url https://s3.fr-par.scw.cloud

# Create snapshot
scw instance snapshot create \
  name=talos-v1.9.0 \
  volume-type=l_ssd \
  bucket=my-bucket \
  key=talos-amd64.qcow2 \
  zone=fr-par-1

# Create image from snapshot
scw instance image create \
  name=talos-v1.9.0 \
  snapshot-id=<snapshot-id> \
  arch=x86_64 \
  zone=fr-par-1

# --- arm64 ---
aws s3 cp talos-arm64.qcow2 s3://my-bucket/talos-arm64.qcow2 \
  --endpoint-url https://s3.fr-par.scw.cloud

scw instance snapshot create \
  name=talos-v1.9.0 \
  volume-type=l_ssd \
  bucket=my-bucket \
  key=talos-arm64.qcow2 \
  zone=fr-par-1

scw instance image create \
  name=talos-v1.9.0 \
  snapshot-id=<snapshot-id> \
  arch=arm64 \
  zone=fr-par-1
```

Repeat for each zone. Because the provider filters images by both name and arch, amd64 and arm64 instances can share the same `image_name` — the correct image is selected automatically.

## Running

The provider is distributed as a container image via GHCR.

```bash
docker run --rm \
  -e SCW_ACCESS_KEY=<your-access-key> \
  -e SCW_SECRET_KEY=<your-secret-key> \
  -e SCW_DEFAULT_PROJECT_ID=<your-project-id> \
  -e OMNI_SERVICE_ACCOUNT_KEY=<your-service-account-key> \
  ghcr.io/coler-e/omni-infra-provider-scaleway:latest \
  --omni-api-endpoint=https://<your-omni-host>:443
```

### CLI flags

| Flag | Default | Description |
|------|---------|-------------|
| `--omni-api-endpoint` | required | Omni gRPC endpoint (e.g. `https://omni.example.com:443`). |
| `--default-zone` | `fr-par-1` | Fallback zone when no `zone` or `zones` is set in provider data. |
| `--insecure-skip-verify` | `false` | Skip TLS verification for the Omni endpoint (dev only). |

### Environment variables

| Variable | Description |
|----------|-------------|
| `SCW_ACCESS_KEY` | Scaleway API access key. |
| `SCW_SECRET_KEY` | Scaleway API secret key. |
| `SCW_DEFAULT_PROJECT_ID` | Scaleway project ID. |
| `OMNI_SERVICE_ACCOUNT_KEY` | Omni service account key (base64 JSON). |

## Machine Class Configuration

Provider data is set in Omni as a YAML blob on the `MachineRequest` or `MachineClass` resource.

### Full example

```yaml
commercial_type: DEV1-M
image_name: talos-v1.9.0
disk_size_gb: 40
zones:
  - fr-par-1
  - fr-par-2
  - fr-par-3
  - nl-ams-1
  - pl-waw-1
private_network_ids:
  fr-par: pn-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
  nl-ams: pn-yyyyyyyy-yyyy-yyyy-yyyy-yyyyyyyyyyyy
  pl-waw: pn-zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz
tags:
  - env=prod
```

### Provider data fields

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `commercial_type` | string | yes | — | Scaleway instance type (e.g. `DEV1-M`, `GP1-XS`, `PRO2-S`). |
| `image_name` | string | one of | — | Base name of a pre-uploaded Talos image. The provider selects the image matching the zone and inferred arch. |
| `image_id` | string | one of | — | Direct UUID of a Scaleway image (zone-specific, no arch inference). |
| `zones` | []string | no | — | List of zones to spread machines across with guaranteed even distribution. Takes precedence over `zone`. |
| `zone` | string | no | provider default | Single zone for all machines. |
| `arch` | string | no | inferred | Override architecture: `amd64` or `arm64`. Inferred automatically from the instance type if not set. |
| `disk_size_gb` | integer | no | `40` | Root disk size in GB. Must respect the instance type's local SSD limits. |
| `private_network_ids` | object | no | — | Map of Scaleway region (e.g. `fr-par`) to Private Network UUID. Instances are attached to the matching network at creation. |
| `tags` | []string | no | — | Additional tags applied to the Scaleway instance. |

### Zone spread

When `zones` is set, the provider distributes machines across zones with guaranteed even spread.
The first machine goes to `zones[0]`, the second to `zones[1]`, and so on.
Count-based selection ensures real balance at small scale (unlike hash-based selection).

### Private Networks

Scaleway Private Networks are regional — they span all zones within a region (e.g. `fr-par-1`, `fr-par-2`, `fr-par-3` share the same regional private network).
Use `private_network_ids` to map each region to its private network.
Regions without an entry receive no private NIC (public network only).

## Building from Source

```bash
make image-omni-infra-provider-scaleway USERNAME=<your-github-user> REGISTRY=ghcr.io
```

To push directly:

```bash
make image-omni-infra-provider-scaleway USERNAME=<your-github-user> REGISTRY=ghcr.io PUSH=true
```

## License

Mozilla Public License 2.0.

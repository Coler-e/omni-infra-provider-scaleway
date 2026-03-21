# omni-infra-provider-scaleway

An [Omni](https://github.com/siderolabs/omni) infrastructure provider for [Scaleway](https://www.scaleway.com/).
It provisions Scaleway Instances on demand and registers them with Omni as Talos machines.

## Features

- Automatic provisioning and deprovisioning of Scaleway Instances.
- Multi-zone distribution with guaranteed even spread (count-based zone selection, atomic under concurrency).
- Multi-region support: fr-par, nl-ams, pl-waw, it-mil.
- Per-zone and per-region instance type overrides — use different `commercial_type` values across regions.
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

### Docker Compose (recommended)

Create a `.env` file with your credentials:

```env
SCW_ACCESS_KEY=<your-access-key>
SCW_SECRET_KEY=<your-secret-key>
SCW_DEFAULT_PROJECT_ID=<your-project-id>
SCW_DEFAULT_ZONE=fr-par-1
OMNI_ENDPOINT=https://<your-omni-host>:443
OMNI_SERVICE_ACCOUNT_KEY=<your-service-account-key>
```

Then start the provider:

```bash
docker compose up -d
```

### Docker

```bash
docker run -d \
  -e SCW_ACCESS_KEY=<your-access-key> \
  -e SCW_SECRET_KEY=<your-secret-key> \
  -e SCW_DEFAULT_PROJECT_ID=<your-project-id> \
  -e SCW_DEFAULT_ZONE=fr-par-1 \
  -e OMNI_ENDPOINT=https://<your-omni-host>:443 \
  -e OMNI_SERVICE_ACCOUNT_KEY=<your-service-account-key> \
  ghcr.io/coler-e/omni-infra-provider-scaleway:latest
```

### CLI flags

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--omni-api-endpoint` | `OMNI_ENDPOINT` | required | Omni gRPC endpoint (e.g. `https://omni.example.com:443`). |
| `--omni-service-account-key` | `OMNI_SERVICE_ACCOUNT_KEY` | — | Omni service account key. |
| `--scaleway-access-key` | `SCW_ACCESS_KEY` | — | Scaleway API access key. |
| `--scaleway-secret-key` | `SCW_SECRET_KEY` | — | Scaleway API secret key. |
| `--scaleway-project-id` | `SCW_DEFAULT_PROJECT_ID` | — | Scaleway project ID. |
| `--scaleway-default-zone` | `SCW_DEFAULT_ZONE` | `fr-par-1` | Fallback zone when no `zone` or `regions` is set in provider data. |
| `--insecure-skip-verify` | — | `false` | Skip TLS verification for the Omni endpoint (dev only). |


## Machine Class Configuration

Provider data is set in Omni as a YAML blob on the `MachineRequest` or `MachineClass` resource.

### Full example

```yaml
# Global defaults
commercial_type: GP1-M          # used when no region/zone override is set
image_name: talos-v1.12.6
disk_size_gb: 40
tags:
  - env=prod

regions:
  - region: fr-par
    commercial_type: PRO2-M     # overrides global for all fr-par zones
    network_id: pn-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
    zones:
      - zone: fr-par-1
      - zone: fr-par-2
      - zone: fr-par-3
        commercial_type: DEV1-L # overrides region for this zone only

  - region: nl-ams
    network_id: pn-yyyyyyyy-yyyy-yyyy-yyyy-yyyyyyyyyyyy
    zones:
      - zone: nl-ams-1          # falls back to global GP1-M
      - zone: nl-ams-2
      - zone: nl-ams-3

  - region: pl-waw
    zones:
      - zone: pl-waw-1
      - zone: pl-waw-2
      - zone: pl-waw-3
```

### Provider data fields

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `commercial_type` | string | yes | — | Default Scaleway instance type (e.g. `GP1-M`, `PRO2-S`). Can be overridden per region or per zone. |
| `image_name` | string | one of | — | Base name of a pre-uploaded Talos image. The provider selects the image matching the zone arch automatically. |
| `image_id` | string | one of | — | Direct UUID of a Scaleway image (zone-specific, no arch inference). |
| `zone` | string | no | provider default | Single zone for all machines. Ignored when `regions` is set. |
| `regions` | array | no | — | List of region entries for multi-zone/multi-region distribution. Takes precedence over `zone`. |
| `regions[].region` | string | yes | — | Scaleway region: `fr-par`, `nl-ams`, `pl-waw`, or `it-mil`. |
| `regions[].zones` | array | yes | — | Zones within this region. Each entry is an object with a `zone` field. |
| `regions[].zones[].zone` | string | yes | — | Scaleway zone (e.g. `fr-par-1`). Must belong to the parent region. |
| `regions[].zones[].commercial_type` | string | no | — | Instance type for this zone. Overrides region and global `commercial_type`. |
| `regions[].commercial_type` | string | no | — | Instance type for all zones in this region. Overrides global `commercial_type`. |
| `regions[].network_id` | string | no | — | Private Network UUID to attach instances to. Scaleway Private Networks span all zones in a region. |
| `arch` | string | no | inferred | Override architecture: `amd64` or `arm64`. Inferred automatically from the instance type if not set. |
| `disk_size_gb` | integer | no | `40` | Root disk size in GB. Must respect the instance type's local SSD limits. |
| `tags` | []string | no | — | Additional tags applied to the Scaleway instance. |

### Instance type precedence

When a machine is placed in a zone, the provider resolves `commercial_type` in this order:

1. **Zone-level** `regions[].zones[].commercial_type` (most specific)
2. **Region-level** `regions[].commercial_type`
3. **Global** `commercial_type` (fallback)

This allows different regions to use different instance families (e.g. `PRO2-M` in `fr-par` where it is available, `GP1-M` elsewhere).

### Zone spread

When `regions` is set, the provider collects all declared zones and distributes machines across them with guaranteed even spread. Zone counts are tracked in-memory and updated atomically on each provisioning, so concurrent requests are always balanced.

### Private Networks

Scaleway Private Networks are regional — they span all zones within a region.
Set `network_id` on a region entry to attach every instance in that region to the specified private network.
Regions without a `network_id` use the public network only.

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

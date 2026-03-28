# omni-infra-provider-scaleway

> **⚠️ Disclaimer:** This provider was vibe-coded. While a number of edge cases have been addressed along the way, it has not been through rigorous production hardening. Use at your own risk.

An [Omni](https://github.com/siderolabs/omni) infrastructure provider for [Scaleway](https://www.scaleway.com/).
It provisions Scaleway Instances on demand and registers them with Omni as Talos machines.

## Features

- Automatic provisioning and deprovisioning of Scaleway Instances.
- Multi-zone distribution with guaranteed even spread (count-based zone selection, atomic under concurrency).
- Multi-region support: fr-par, nl-ams, pl-waw, it-mil.
- Per-zone and per-region instance type overrides — use different `commercial_type` values across regions.
- Automatic architecture inference from Scaleway's server-types API (no manual `arch` required).
- Image lookup by name with arch filter — one machine class works across all zones.
- Block-storage instance types (POP2, BASIC2, …) supported via SBS snapshots — provider selects `<image_name>-sbs` automatically.
- Private Network attachment per region (Scaleway VPCs span zones within a region).
- QEMU guest agent pre-installed in every schematic.
- `omni-request-id` tagging for traceability.

## Prerequisites

- A running [Omni](https://omni.siderolabs.com/) instance (self-hosted or SaaS).
- A Scaleway account with an API key (access key + secret key).
- A Talos OS image uploaded to Scaleway (one per zone). See [Image Setup](#image-setup).
- An Omni service account key for the provider.

## Image Setup

Before using this provider you must build a Talos image for Scaleway, upload it to Scaleway Object Storage, and import it as a custom image in each zone you intend to use.

For detailed Scaleway-specific guidance see the official Talos documentation:
[Talos Linux — Scaleway Platform](https://docs.siderolabs.com/talos/v1.12/platform-specific-installations/cloud-platforms/scaleway/)

### Using the helper script

The repository includes a script that automates the full workflow (download → convert → upload → register) for one or both architectures across any set of zones.

The script creates **two image variants per zone**:
- `<image-name>` — backed by an `l_ssd` snapshot, for traditional instance types (GP1, DEV1, PRO2, COPARM1, …)
- `<image-name>-sbs` — backed by a block-storage (SBS) snapshot, for block-storage instance types (POP2, BASIC2, …). Skipped with a warning for zones where the block storage API is unavailable.

The provider automatically selects the correct variant based on the `commercial_type` you configure.

```bash
# Full run: both amd64 + arm64, all 9 zones (fr-par, nl-ams, pl-waw)
./hack/upload-talos-image.sh \
  --version v1.12.6 \
  --schematic 271b03e6560e1dc33065909a4613f1b99bc34224ce6f9991604bbf615218aa6a

# amd64 only, specific zones
./hack/upload-talos-image.sh \
  --version v1.12.6 \
  --schematic 271b03e6560e1dc33065909a4613f1b99bc34224ce6f9991604bbf615218aa6a \
  --arch amd64 \
  --zones fr-par-1,fr-par-2,fr-par-3

# Custom image name (default: talos-<version>)
./hack/upload-talos-image.sh --version v1.13.0 --image-name talos-v1.13.0
```

The script reads `SCW_ACCESS_KEY`, `SCW_SECRET_KEY`, and `SCW_DEFAULT_PROJECT_ID` from the environment or from a `.env` file in the current directory.
Run `./hack/upload-talos-image.sh --help` for all options.

> **Bucket naming:** the script expects buckets named `talos-images-omni` (fr-par), `talos-images-omni-nl` (nl-ams), and `talos-images-omni-pl` (pl-waw). Edit `REGION_BUCKETS` at the top of the script to match your setup.

---

### Manual process

### Schematic requirements

Scaleway instances require a schematic that includes:

- **QEMU guest agent** (`siderolabs/qemu-guest-agent`) — required for Scaleway's hypervisor to interact with the VM (soft reboot, shutdown signals, metrics).
- **Scaleway kernel args** — required so the Talos bootloader is configured correctly for the platform. Without these, kernel-arg upgrades will wipe platform support.

The correct customization block is:

```yaml
customization:
  extraKernelArgs:
    - -console
    - talos.platform=scaleway
    - console=ttyS0,115200
    - talos.dashboard.disabled=0
  systemExtensions:
    officialExtensions:
      - siderolabs/qemu-guest-agent
```

Generate this schematic at [factory.talos.dev](https://factory.talos.dev/) (select *Scaleway* as the platform, add the QEMU extension, and confirm the kernel args). The resulting schematic ID is embedded in the image download URL.

### Downloading and converting the image

The factory provides images in compressed raw format (`.raw.zst`). Scaleway's snapshot import requires `.qcow2` or `.raw`, so convert before uploading.
Repeat for each architecture (`amd64` / `arm64`) you intend to use:

```bash
SCHEMATIC="271b03e6560e1dc33065909a4613f1b99bc34224ce6f9991604bbf615218aa6a"
TALOS_VERSION="v1.12.6"
ARCH="amd64"   # or arm64

# Download
curl -L -o "scaleway-${ARCH}.raw.zst" \
  "https://factory.talos.dev/image/${SCHEMATIC}/${TALOS_VERSION}/scaleway-${ARCH}.raw.zst"

# Decompress
zstd -d "scaleway-${ARCH}.raw.zst" -o "scaleway-${ARCH}.raw"

# Convert to qcow2 (Scaleway snapshot import format)
qemu-img convert -f raw -O qcow2 "scaleway-${ARCH}.raw" "scaleway-${ARCH}.qcow2"
```

### Uploading and registering the image (per zone)

Create an [Object Storage bucket](https://www.scaleway.com/en/object-storage/) in each region, upload the image, then import it as a snapshot and create an image. Repeat for every zone you plan to use.

You need two image variants:

#### l_ssd image (GP1, DEV1, PRO2, COPARM1, …)

```bash
BUCKET="my-talos-images"       # bucket name (must be in the same region as the zone)
IMAGE_NAME="talos-v1.12.6"     # must match image_name in your machine class
ARCH="amd64"                   # or arm64
SCW_ARCH="x86_64"              # x86_64 for amd64, arm64 for arm64

# Upload to Object Storage (example: fr-par region)
aws s3 cp "scaleway-${ARCH}.qcow2" "s3://${BUCKET}/scaleway-${ARCH}.qcow2" \
  --endpoint-url https://s3.fr-par.scw.cloud

# Import as l_ssd snapshot (one per zone)
SNAP_ID=$(scw instance snapshot create \
  name="${IMAGE_NAME}-${ARCH}-snap" \
  volume-type=l_ssd \
  bucket="${BUCKET}" \
  key="scaleway-${ARCH}.qcow2" \
  zone=fr-par-1 \
  --output json | jq -r '.snapshot.id')

# Wait until state == available, then create the image
scw instance image create \
  name="${IMAGE_NAME}" \
  snapshot-id="${SNAP_ID}" \
  arch="${SCW_ARCH}" \
  zone=fr-par-1
```

#### SBS image (POP2, BASIC2, …)

Block-storage instance types require a snapshot created via the block API (not the instance API).
The script attempts this for every zone and skips with a warning if the zone does not support block storage.

```bash
# Import as block (SBS) snapshot via the block API
SNAP_ID=$(scw block snapshot import-from-object-storage \
  bucket="${BUCKET}" \
  key="scaleway-${ARCH}.qcow2" \
  name="${IMAGE_NAME}-sbs-${ARCH}-snap" \
  project-id="${SCW_DEFAULT_PROJECT_ID}" \
  zone=fr-par-1 \
  --output json | jq -r '.id')

# Wait until status == available
# scw block snapshot get "${SNAP_ID}" zone=fr-par-1 --output json | jq '.status'

# Create instance image from the block snapshot (name must end in -sbs)
scw instance image create \
  name="${IMAGE_NAME}-sbs" \
  snapshot-id="${SNAP_ID}" \
  arch="${SCW_ARCH}" \
  zone=fr-par-1
```

Because the provider filters images by both name and architecture, amd64 and arm64 instances can share the same `image_name` value in your machine class — the correct image variant is selected automatically per zone and instance type.

## Running

The provider is distributed as a container image via GHCR.

### Docker Compose (recommended)

Create a `.env` file with your credentials (see `.env.example`):

```env
SCW_ACCESS_KEY=<your-access-key>
SCW_SECRET_KEY=<your-secret-key>
SCW_DEFAULT_PROJECT_ID=<your-project-id>
SCW_DEFAULT_ZONE=fr-par-1
OMNI_ENDPOINT=https://<your-omni-host>:443
OMNI_SERVICE_ACCOUNT_KEY=<your-service-account-key>
PROVIDER_ID=scaleway
```

> **Provider ID:** `PROVIDER_ID` must match the ID used when creating the Omni service account for this provider. Each provider instance must have a unique ID (e.g. `scaleway-fr-par`, `scaleway-nl-ams`).

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
  -e PROVIDER_ID=scaleway \
  ghcr.io/coler-e/omni-infra-provider-scaleway:latest
```

### CLI flags

| Flag | Env var | Default | Description |
|------|---------|---------|-------------|
| `--id` | `PROVIDER_ID` | `scaleway` | Provider ID. Must match the ID used when creating the Omni service account. Each instance needs a unique ID. |
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
# Global defaults (apply to all zones unless overridden)
commercial_type: GP1-M
image_name: talos-v1.12.6
disk_size_gb: 40
tags:
  - env=prod

regions:
  - region: fr-par
    commercial_type: PRO2-M     # overrides global for all fr-par zones
    disk_size_gb: 80            # overrides global for all fr-par zones
    network_id: pn-xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx
    zones:
      - zone: fr-par-1
      - zone: fr-par-2
      - zone: fr-par-3
        commercial_type: DEV1-L # overrides region for this zone only
        disk_size_gb: 25        # overrides region for this zone only

  - region: nl-ams
    network_id: pn-yyyyyyyy-yyyy-yyyy-yyyy-yyyyyyyyyyyy
    zones:
      - zone: nl-ams-1          # falls back to global GP1-M / 40 GB
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
| `regions[].zones[].zone` | string | yes | — | Scaleway zone (e.g. `fr-par-1`, `fr-par-2`). Must belong to the parent region. |
| `regions[].zones[].commercial_type` | string | no | — | Instance type for this zone. Overrides region and global `commercial_type`. |
| `regions[].zones[].disk_size_gb` | integer | no | — | Disk size in GB for this zone. Overrides region and global `disk_size_gb`. |
| `regions[].commercial_type` | string | no | — | Instance type for all zones in this region. Overrides global `commercial_type`. |
| `regions[].disk_size_gb` | integer | no | — | Disk size in GB for all zones in this region. Overrides global `disk_size_gb`. |
| `regions[].network_id` | string | no | — | Private Network UUID to attach instances to. Scaleway Private Networks span all zones in a region. |
| `arch` | string | no | inferred | Override architecture: `amd64` or `arm64`. Inferred automatically from the instance type if not set. |
| `disk_size_gb` | integer | no | `40` | Default root disk size in GB. Must respect the instance type's local SSD limits. Can be overridden per region or per zone. |
| `tags` | []string | no | — | Additional tags applied to the Scaleway instance. |

### Field precedence

`commercial_type` and `disk_size_gb` both follow the same resolution order:

1. **Zone-level** (most specific)
2. **Region-level**
3. **Global** (fallback)

This is useful when an instance family is only available in certain regions (e.g. `PRO2-M` in `fr-par` but not in `nl-ams`) or when different regions have different local SSD limits.

### Zone spread

When `regions` is set, the provider collects all declared zones and distributes machines across them with guaranteed even spread. Zone counts are tracked in-memory and updated atomically on each provisioning, so concurrent requests are always balanced.

### Private Networks

Scaleway Private Networks are regional — they span all zones within a region.
Set `network_id` on a region entry to attach every instance in that region to the specified private network.
Regions without a `network_id` use the public network only.

## Cloud Controller Manager & CSI Driver

To fully integrate your cluster with Scaleway (LoadBalancer Services, persistent volumes), you need the Scaleway Cloud Controller Manager (CCM) and the Scaleway CSI driver. The patches and instructions below are provided as a convenience starting point — always refer to the upstream repositories for up-to-date configuration, version compatibility, and release notes:

- **CCM**: https://github.com/scaleway/scaleway-cloud-controller-manager
- **CSI**: https://github.com/scaleway/scaleway-csi

### Step 1 — enable external cloud provider on every node

Apply the machine patch `omni-patches/patch-machine-cloud-provider.yaml` to your cluster in Omni (as a machine config patch on the cluster or machine class). This tells kubelet to defer node initialization to the CCM:

```yaml
machine:
  kubelet:
    extraArgs:
      cloud-provider: external
```

### Step 2 — install CCM and CSI

#### Option A: Omni inline manifests (quick start)

The repository includes ready-to-use Omni cluster patches under `omni-patches/`:

| File | What it installs |
|------|-----------------|
| `patch-cluster-ccm.yaml` | CCM RBAC, Secret, Deployment |
| `patch-cluster-csi.yaml` | CSI RBAC, Secret, StorageClasses, DaemonSet, Controller Deployment |

Apply them to your cluster via the Omni UI or CLI. Before applying, **replace the placeholder credentials** in each patch:

```yaml
stringData:
  SCW_ACCESS_KEY: "YOUR_ACCESS_KEY"       # ← replace
  SCW_SECRET_KEY: "YOUR_SECRET_KEY"       # ← replace
  SCW_DEFAULT_PROJECT_ID: "YOUR_PROJECT_ID"  # ← replace
  SCW_DEFAULT_ZONE: "fr-par-1"            # ← set your primary zone
```

The CSI patch pins specific sidecar versions (`csi-provisioner:v5.3.0`, etc.) — check the [scaleway-csi releases](https://github.com/scaleway/scaleway-csi/releases) for the latest recommended versions.

> **Note:** Embedding credentials in Omni patches stores them in Omni's state as plaintext. This is fine for development but consider the alternatives below for production.

#### Option B: Helm

The CSI driver has an official Helm chart which handles versioning and credentials more cleanly:

```bash
helm repo add scaleway https://helm.scw.cloud/
helm repo update

helm upgrade --install scaleway-csi scaleway/scaleway-csi \
  --namespace kube-system \
  --set secret.accessKey=YOUR_ACCESS_KEY \
  --set secret.secretKey=YOUR_SECRET_KEY \
  --set secret.projectId=YOUR_PROJECT_ID \
  --set secret.region=fr-par \
  --set secret.zone=fr-par-1
```

The CCM does not currently have an official Helm chart; apply `patch-cluster-ccm.yaml` directly or manage it via GitOps.

#### Option C: GitOps (Argo CD)

For production, manage both components through a GitOps tool like [Argo CD](https://argo-cd.readthedocs.io/):

1. Store the CCM and CSI manifests (or Helm chart values) in a Git repository.
2. Store credentials in a secret manager (Vault, AWS SSM, Bitwarden, …) and sync them into the cluster via an operator (External Secrets Operator, Sealed Secrets, …).
3. Create Argo `Application` resources pointing at your manifests — Argo will reconcile them on every push.

This approach keeps credentials out of Omni's state and gives you full audit history and rollback for every component change.

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

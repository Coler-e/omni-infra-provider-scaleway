#!/usr/bin/env bash
# upload-talos-image.sh — Download, convert, and register a Talos image for Scaleway.
#
# Usage:
#   ./hack/upload-talos-image.sh [OPTIONS]
#
# Options:
#   --version      Talos version, e.g. v1.12.6           (default: v1.12.6)
#   --schematic    Image factory schematic ID             (default: see RECOMMENDED_SCHEMATIC below)
#   --arch         amd64, arm64, or both                  (default: both)
#   --image-name   Scaleway image name                    (default: talos-<version>)
#   --zones        Comma-separated list of zones          (default: all 9 zones across 3 regions)
#   --workdir      Temp directory for image files         (default: /tmp/talos-image-upload)
#   --keep         Do not delete working files on exit
#   --help
#
# Required environment variables (or set in .env in the current directory):
#   SCW_ACCESS_KEY
#   SCW_SECRET_KEY
#   SCW_DEFAULT_PROJECT_ID
#
# Bucket naming convention (must exist):
#   talos-images-omni       (fr-par region)
#   talos-images-omni-nl    (nl-ams region)
#   talos-images-omni-pl    (pl-waw region)
# Adjust REGION_BUCKETS below if your bucket names differ.
#
# Dependencies: curl, zstd, qemu-img, aws (AWS CLI), scw (Scaleway CLI), jq
#
# Two image variants are created per zone:
#   <image-name>       → l_ssd snapshot, for GP1/DEV1/PRO2/COPARM1 instances
#   <image-name>-sbs   → block (SBS) snapshot, for POP2/BASIC2 instances
#                        Only created in zones where block storage is available.

set -euo pipefail

# ── Recommended schematic ────────────────────────────────────────────────────
# Includes: siderolabs/qemu-guest-agent + Scaleway kernel args
#   (-console, talos.platform=scaleway, console=ttyS0,115200, talos.dashboard.disabled=0)
RECOMMENDED_SCHEMATIC="271b03e6560e1dc33065909a4613f1b99bc34224ce6f9991604bbf615218aa6a"

# ── Defaults ─────────────────────────────────────────────────────────────────
TALOS_VERSION="v1.12.6"
SCHEMATIC="${RECOMMENDED_SCHEMATIC}"
ARCH="both"
IMAGE_NAME=""
WORKDIR="/tmp/talos-image-upload"
KEEP=false
DEFAULT_ZONES="fr-par-1,fr-par-2,fr-par-3,nl-ams-1,nl-ams-2,nl-ams-3,pl-waw-1,pl-waw-2,pl-waw-3"
ZONES=""

# ── Bucket config (region → bucket name + S3 endpoint) ───────────────────────
declare -A REGION_BUCKETS=(
  [fr-par]="talos-images-omni"
  [nl-ams]="talos-images-omni-nl"
  [pl-waw]="talos-images-omni-pl"
)

declare -A REGION_ENDPOINTS=(
  [fr-par]="https://s3.fr-par.scw.cloud"
  [nl-ams]="https://s3.nl-ams.scw.cloud"
  [pl-waw]="https://s3.pl-waw.scw.cloud"
)


# ── Parse arguments ──────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)   TALOS_VERSION="$2"; shift 2 ;;
    --schematic) SCHEMATIC="$2";     shift 2 ;;
    --arch)      ARCH="$2";          shift 2 ;;
    --image-name) IMAGE_NAME="$2";   shift 2 ;;
    --zones)     ZONES="$2";         shift 2 ;;
    --workdir)   WORKDIR="$2";       shift 2 ;;
    --keep)      KEEP=true;          shift   ;;
    --help|-h)
      grep '^#' "$0" | head -35 | sed 's/^# \?//'
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      exit 1
      ;;
  esac
done

# ── Validate arch ────────────────────────────────────────────────────────────
case "${ARCH}" in
  amd64|arm64|both) ;;
  *) echo "ERROR: --arch must be amd64, arm64, or both" >&2; exit 1 ;;
esac

# ── Image name default ───────────────────────────────────────────────────────
if [[ -z "${IMAGE_NAME}" ]]; then
  IMAGE_NAME="talos-${TALOS_VERSION}"
fi

# ── Zone list ────────────────────────────────────────────────────────────────
if [[ -z "${ZONES}" ]]; then
  ZONES="${DEFAULT_ZONES}"
fi
IFS=',' read -ra ZONE_LIST <<< "${ZONES}"

# ── Load credentials from .env if not already set ────────────────────────────
if [[ -f ".env" ]]; then
  # shellcheck disable=SC2046
  export $(grep -v '^\s*#' .env | grep -v '^\s*$' | xargs)
fi

for VAR in SCW_ACCESS_KEY SCW_SECRET_KEY SCW_DEFAULT_PROJECT_ID; do
  if [[ -z "${!VAR:-}" ]]; then
    echo "ERROR: ${VAR} is not set. Export it or add it to .env" >&2
    exit 1
  fi
done

export SCW_ACCESS_KEY SCW_SECRET_KEY SCW_DEFAULT_PROJECT_ID

# ── Dependency check ─────────────────────────────────────────────────────────
for dep in curl zstd qemu-img aws scw jq; do
  if ! command -v "${dep}" &>/dev/null; then
    echo "ERROR: required tool not found: ${dep}" >&2
    exit 1
  fi
done

# ── Setup ────────────────────────────────────────────────────────────────────
mkdir -p "${WORKDIR}"

if [[ "${KEEP}" == "false" ]]; then
  trap 'echo "Cleaning up ${WORKDIR}..."; rm -rf "${WORKDIR}"' EXIT
fi

# ── Helper: derive region from zone (e.g. fr-par-1 → fr-par) ────────────────
zone_to_region() {
  echo "${1}" | sed 's/-[0-9]*$//'
}

# ── Helper: download + convert one arch ──────────────────────────────────────
prepare_image() {
  local arch="$1"
  local raw_zst="${WORKDIR}/scaleway-${arch}.raw.zst"
  local raw="${WORKDIR}/scaleway-${arch}.raw"
  local qcow2="${WORKDIR}/scaleway-${arch}.qcow2"
  local url="https://factory.talos.dev/image/${SCHEMATIC}/${TALOS_VERSION}/scaleway-${arch}.raw.zst"

  if [[ -f "${qcow2}" ]]; then
    echo "  [${arch}] qcow2 already exists in workdir, skipping download." >&2
    return
  fi

  echo "  [${arch}] Downloading from factory.talos.dev..." >&2
  curl -L --progress-bar -o "${raw_zst}" "${url}"

  echo "  [${arch}] Decompressing .raw.zst → .raw ..." >&2
  zstd -d "${raw_zst}" -o "${raw}" --force
  rm -f "${raw_zst}"

  echo "  [${arch}] Converting .raw → .qcow2 ..." >&2
  qemu-img convert -f raw -O qcow2 -p "${raw}" "${qcow2}"
  rm -f "${raw}"

  echo "  [${arch}] qcow2 ready ($(du -sh "${qcow2}" | cut -f1))" >&2
}

# ── Helper: upload qcow2 to a region bucket ───────────────────────────────────
upload_image() {
  local arch="$1"
  local region="$2"
  local qcow2="${WORKDIR}/scaleway-${arch}.qcow2"
  local bucket="${REGION_BUCKETS[$region]}"
  local endpoint="${REGION_ENDPOINTS[$region]}"
  local key="scaleway-${arch}.qcow2"

  echo "  [${arch}] Uploading to s3://${bucket}/${key} (${region})..." >&2
  AWS_ACCESS_KEY_ID="${SCW_ACCESS_KEY}" \
  AWS_SECRET_ACCESS_KEY="${SCW_SECRET_KEY}" \
  aws s3 cp "${qcow2}" "s3://${bucket}/${key}" \
    --endpoint-url "${endpoint}" \
    --no-progress
}

# ── Helper: create one l_ssd snapshot + instance image ───────────────────────
# Args: arch zone image_name
# Outputs: "zone|arch|l_ssd|image_id" on stdout (progress to stderr)
_register_lssd() {
  local arch="$1"
  local zone="$2"
  local img_name="$3"
  local region
  region=$(zone_to_region "${zone}")
  local bucket="${REGION_BUCKETS[$region]}"
  local key="scaleway-${arch}.qcow2"
  local scw_arch
  [[ "${arch}" == "amd64" ]] && scw_arch="x86_64" || scw_arch="arm64"

  # Delete existing image with the same name+arch (idempotent re-run)
  local old_id
  old_id=$(scw instance image list zone="${zone}" --output json 2>/dev/null \
    | jq -r --arg n "${img_name}" --arg a "${scw_arch}" \
        '.[] | select(.name == $n and .arch == $a) | .id' 2>/dev/null | head -1)

  if [[ -n "${old_id}" ]]; then
    local old_snap
    old_snap=$(scw instance image get "${old_id}" zone="${zone}" --output json 2>/dev/null \
      | jq -r '.root_volume.id' 2>/dev/null)
    echo "    [${arch}] Removing existing l_ssd image ${old_id} (${img_name})..." >&2
    scw instance image delete "${old_id}" zone="${zone}" with-snapshots=false >/dev/null 2>&1 || true
    if [[ -n "${old_snap}" && "${old_snap}" != "null" ]]; then
      scw instance snapshot delete "${old_snap}" zone="${zone}" >/dev/null 2>&1 || true
    fi
  fi

  echo "    [${arch}] Creating l_ssd snapshot from s3://${bucket}/${key}..." >&2
  local snap_id
  snap_id=$(scw instance snapshot create \
    name="${img_name}-${arch}-snap" \
    volume-type="l_ssd" \
    bucket="${bucket}" \
    key="${key}" \
    zone="${zone}" \
    --output json | jq -r '.snapshot.id')

  echo "    [${arch}] Waiting for l_ssd snapshot ${snap_id}..." >&2
  local state
  for _ in $(seq 1 60); do
    state=$(scw instance snapshot get "${snap_id}" zone="${zone}" --output json \
      | jq -r '.snapshot.state')
    [[ "${state}" == "available" ]] && break
    [[ "${state}" == "error" ]] && {
      echo "ERROR: l_ssd snapshot ${snap_id} errored in ${zone}" >&2; return 1
    }
    sleep 30
  done

  local img_id
  img_id=$(scw instance image create \
    name="${img_name}" \
    snapshot-id="${snap_id}" \
    arch="${scw_arch}" \
    zone="${zone}" \
    --output json | jq -r '.image.id')

  echo "    [${arch}] l_ssd image registered: ${img_id} (${img_name})" >&2
  echo "${zone}|${arch}|l_ssd|${img_id}"
}

# ── Helper: create one SBS (block) snapshot + instance image ─────────────────
# Uses the block API (scw block snapshot import-from-object-storage).
# Only available in zones with block storage support; skips other zones.
# Args: arch zone image_name
# Outputs: "zone|arch|sbs_volume|image_id" on stdout (progress to stderr)
_register_sbs() {
  local arch="$1"
  local zone="$2"
  local img_name="$3"
  local region
  region=$(zone_to_region "${zone}")
  local bucket="${REGION_BUCKETS[$region]}"
  local key="scaleway-${arch}.qcow2"
  local scw_arch
  [[ "${arch}" == "amd64" ]] && scw_arch="x86_64" || scw_arch="arm64"

  # Delete existing instance image with same name+arch (idempotent re-run)
  local old_id
  old_id=$(scw instance image list zone="${zone}" --output json 2>/dev/null \
    | jq -r --arg n "${img_name}" --arg a "${scw_arch}" \
        '.[] | select(.name == $n and .arch == $a) | .id' 2>/dev/null | head -1)

  if [[ -n "${old_id}" ]]; then
    local old_snap
    old_snap=$(scw instance image get "${old_id}" zone="${zone}" --output json 2>/dev/null \
      | jq -r '.root_volume.id' 2>/dev/null)
    echo "    [${arch}] Removing existing SBS image ${old_id} (${img_name})..." >&2
    scw instance image delete "${old_id}" zone="${zone}" with-snapshots=false >/dev/null 2>&1 || true
    if [[ -n "${old_snap}" && "${old_snap}" != "null" ]]; then
      # Block snapshots are deleted via the block API
      scw block snapshot delete "${old_snap}" zone="${zone}" >/dev/null 2>&1 || true
    fi
  fi

  echo "    [${arch}] Creating SBS snapshot from s3://${bucket}/${key} via block API..." >&2
  local snap_json snap_id
  if ! snap_json=$(scw block snapshot import-from-object-storage \
    bucket="${bucket}" \
    key="${key}" \
    name="${img_name}-${arch}-snap" \
    project-id="${SCW_DEFAULT_PROJECT_ID}" \
    zone="${zone}" \
    --output json 2>&1); then
    echo "    [${arch}] WARNING: block API not available in ${zone}, skipping SBS image: ${snap_json}" >&2
    return 0
  fi
  snap_id=$(echo "${snap_json}" | jq -r '.id')

  echo "    [${arch}] Waiting for SBS snapshot ${snap_id}..." >&2
  local status
  for _ in $(seq 1 60); do
    status=$(scw block snapshot get "${snap_id}" zone="${zone}" --output json \
      | jq -r '.status')
    [[ "${status}" == "available" || "${status}" == "in_use" ]] && break
    [[ "${status}" == "error" || "${status}" == "locked" ]] && {
      echo "ERROR: SBS snapshot ${snap_id} errored in ${zone} (status: ${status})" >&2; return 1
    }
    sleep 30
  done

  local img_id
  img_id=$(scw instance image create \
    name="${img_name}" \
    snapshot-id="${snap_id}" \
    arch="${scw_arch}" \
    zone="${zone}" \
    --output json | jq -r '.image.id')

  echo "    [${arch}] SBS image registered: ${img_id} (${img_name})" >&2
  echo "${zone}|${arch}|sbs_volume|${img_id}"
}

# ── Helper: register both l_ssd and SBS images for one arch+zone ─────────────
register_image() {
  local arch="$1"
  local zone="$2"

  # l_ssd image — for traditional instance types (GP1, DEV1, PRO2, COPARM1, …)
  _register_lssd "${arch}" "${zone}" "${IMAGE_NAME}"

  # SBS image — for block-storage instance types (POP2, BASIC2, …)
  # Provider looks this up as "<image_name>-sbs" when commercial_type has no local SSD constraint.
  _register_sbs "${arch}" "${zone}" "${IMAGE_NAME}-sbs"
}

# ── Main ──────────────────────────────────────────────────────────────────────
ARCHS=()
[[ "${ARCH}" == "both" || "${ARCH}" == "amd64" ]] && ARCHS+=("amd64")
[[ "${ARCH}" == "both" || "${ARCH}" == "arm64" ]] && ARCHS+=("arm64")

echo "========================================"
echo " Talos image upload for Scaleway"
echo "========================================"
echo "  Version   : ${TALOS_VERSION}"
echo "  Schematic : ${SCHEMATIC}"
echo "  Arch(s)   : ${ARCHS[*]}"
echo "  Image name: ${IMAGE_NAME}"
echo "  Zones     : ${ZONES}"
echo "  Workdir   : ${WORKDIR}"
echo ""

# Step 1: download + convert
echo "── Step 1/3: Download & convert ────────────────────────────────────────"
for arch in "${ARCHS[@]}"; do
  prepare_image "${arch}"
done

# Step 2: upload to each distinct region bucket
echo ""
echo "── Step 2/3: Upload to Object Storage ──────────────────────────────────"
declare -A UPLOADED_REGIONS=()
for zone in "${ZONE_LIST[@]}"; do
  region=$(zone_to_region "${zone}")
  if [[ -z "${UPLOADED_REGIONS[$region]:-}" ]]; then
    for arch in "${ARCHS[@]}"; do
      upload_image "${arch}" "${region}"
    done
    UPLOADED_REGIONS[$region]=1
  fi
done

# Step 3: create snapshot + image per zone
echo ""
echo "── Step 3/3: Register images in Scaleway ───────────────────────────────"
RESULTS=()
for zone in "${ZONE_LIST[@]}"; do
  echo "  Zone: ${zone}"
  for arch in "${ARCHS[@]}"; do
    # register_image prints progress to stderr and result lines to stdout
    while IFS= read -r result_line; do
      [[ -n "${result_line}" ]] && RESULTS+=("${result_line}")
    done < <(register_image "${arch}" "${zone}")
  done
done

# Summary
echo ""
echo "========================================"
echo " Summary"
echo "========================================"
printf "  %-12s  %-8s  %-10s  %s\n" "ZONE" "ARCH" "TYPE" "IMAGE ID"
printf "  %-12s  %-8s  %-10s  %s\n" "----" "----" "----" "--------"
for result in "${RESULTS[@]}"; do
  IFS='|' read -r r_zone r_arch r_vol_type r_img_id <<< "${result}"
  printf "  %-12s  %-8s  %-10s  %s\n" "${r_zone}" "${r_arch}" "${r_vol_type}" "${r_img_id}"
done
echo ""
echo "  image_name: ${IMAGE_NAME}          → l_ssd instances (GP1, DEV1, PRO2, COPARM1, …)"
echo "  image_name: ${IMAGE_NAME}-sbs      → block-storage instances (POP2, BASIC2, …)"
echo "  The provider selects the correct image automatically based on instance type."
echo "  schematic:  ${SCHEMATIC}"

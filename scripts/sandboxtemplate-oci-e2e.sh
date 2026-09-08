#!/usr/bin/env bash
# sandboxtemplate-oci-e2e.sh — exercise ONLY the OverlayBD OCI image
# packaging stage of the builder (attach empty raw volume → byte-exact
# write → commit → push), decoupled from the full golden-image pipeline:
# no KVM, no Firecracker, no S3. This is the isolated seam for iterating
# on the strmvold integration.
#
# Flow:
#   1. fixture images: a sparse rootfs-like file (data + holes) and a dense
#      memory-like file, filled with random data
#   2. sandboxtemplate-builder oci-publish → two single-layer LSMT images
#      pushed to a throwaway registry:2
#   3. registry API assertions: manifest exists, digest matches the pin,
#      layer carries the OverlayBD annotations, config is the empty blob
#   4. byte-exactness roundtrip: strmvolctl attach --readonly the pushed
#      image, read sizeBytes off the device, sha256 must equal the source
#      fixture (holes must read as zeros — the mkfs-residue guard)
#
# Requirements (internal build host):
#   - Linux x86_64, kernel >= 5.19 with ublk (/dev/ublk-control present)
#   - docker (registry:2 container)
#   - the streamingvolume runtime installed on the HOST and running:
#     strmvold (systemd unit from the t-storage-strmvold rpm), the overlaybd
#     api server (127.0.0.1:9862, part of the internal overlaybd runtime),
#     /opt/overlaybd toolchain, and strmvolctl on PATH
#   - go toolchain (builds the builder binary)
#
# Usage:
#   ./scripts/sandboxtemplate-oci-e2e.sh [--skip-roundtrip] [--keep]

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$PWD/.sandboxtemplate-oci-e2e}"
REGISTRY_PORT="${REGISTRY_PORT:-15000}"
REGISTRY="127.0.0.1:${REGISTRY_PORT}"
SKIP_ROUNDTRIP=0
KEEP=0

log() { printf '\033[1;34m[st-oci-e2e]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[st-oci-e2e] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --skip-roundtrip) SKIP_ROUNDTRIP=1; shift ;;
        --keep) KEEP=1; shift ;;
        *) die "unknown argument: $1" ;;
    esac
done

[[ "$(uname -s)" == "Linux" ]] || die "requires Linux (ublk/strmvold stack)"
[[ "$(uname -m)" == "x86_64" ]] || die "requires x86_64"
[[ -e /dev/ublk-control ]] || die "/dev/ublk-control missing (kernel >= 5.19 with ublk required)"
command -v docker >/dev/null || die "missing required command: docker"
command -v go >/dev/null || die "missing required command: go"
command -v strmvolctl >/dev/null || die "missing strmvolctl on PATH (t-storage-strmvold rpm)"
command -v jq >/dev/null || die "missing required command: jq"
command -v sha256sum >/dev/null || die "missing required command: sha256sum"

# The builder's own strmvold session (for oci-publish) and the host daemon
# (for the roundtrip) both need the overlaybd api server.
if ! curl -fsS -m 3 "http://127.0.0.1:9862/" >/dev/null 2>&1 && ! curl -fsS -m 3 -X POST "http://127.0.0.1:9862/" >/dev/null 2>&1; then
    # Any response (even 404/405) proves a listener; a connection refused
    # means the api server is down.
    if ! (echo > /dev/tcp/127.0.0.1/9862) 2>/dev/null; then
        die "overlaybd api server not reachable on 127.0.0.1:9862 — start the overlaybd runtime first"
    fi
fi

log "workspace: $WORK"
[[ "$KEEP" == 1 ]] || trap 'rm -rf "$WORK"' EXIT
rm -rf "$WORK"
mkdir -p "$WORK"

# --- throwaway registry -------------------------------------------------------
log "starting registry:2 on ${REGISTRY}"
docker run -d --rm --name st-oci-e2e-registry -p "${REGISTRY_PORT}:5000" registry:2 >/dev/null
[[ "$KEEP" == 1 ]] || trap 'docker stop st-oci-e2e-registry >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT
for _ in $(seq 1 30); do
    curl -fsS "http://${REGISTRY}/v2/" >/dev/null 2>&1 && break
    sleep 1
done
curl -fsS "http://${REGISTRY}/v2/" >/dev/null || die "registry did not come up"

# --- fixtures -----------------------------------------------------------------
# rootfs-like: 1GiB sparse with random extents and holes; memory-like: 64MiB dense.
ROOTFS_FIXTURE="$WORK/rootfs.fixture"
MEMORY_FIXTURE="$WORK/memory.fixture"
log "generating fixtures"
truncate -s 1G "$ROOTFS_FIXTURE"
dd if=/dev/urandom of="$ROOTFS_FIXTURE" bs=1M count=8 conv=notrunc status=none
dd if=/dev/urandom of="$ROOTFS_FIXTURE" bs=1M count=8 seek=64 conv=notrunc status=none
dd if=/dev/urandom of="$MEMORY_FIXTURE" bs=1M count=64 status=none
ROOTFS_SHA=$(sha256sum "$ROOTFS_FIXTURE" | awk '{print $1}')
MEMORY_SHA=$(sha256sum "$MEMORY_FIXTURE" | awk '{print $1}')
MEMORY_SIZE=$(stat -c%s "$MEMORY_FIXTURE")
log "fixture digests: rootfs=$ROOTFS_SHA memory=$MEMORY_SHA"

# --- oci-publish --------------------------------------------------------------
log "building sandboxtemplate-builder"
(cd "$REPO_ROOT" && CGO_ENABLED=0 go build -o "$WORK/sandboxtemplate-builder" ./cmd/sandboxtemplate-builder/)

log "running oci-publish against ${REGISTRY}"
if ! SANDBOX_TEMPLATE_REGISTRY_PLAINHTTP=1 \
    "$WORK/sandboxtemplate-builder" oci-publish \
        --rootfs "$ROOTFS_FIXTURE" \
        --memory "$MEMORY_FIXTURE" \
        --registry "${REGISTRY}/e2e/templates/t1" \
        --tag e2e \
        --workdir "$WORK/publish-workdir" > "$WORK/refs.txt" 2> "$WORK/oci-publish.err"; then
    tail -100 "$WORK/oci-publish.err" >&2
    die "oci-publish failed"
fi
ROOTFS_REF="${REGISTRY}/e2e/templates/t1-rootfs:e2e"
MEMORY_REF="${REGISTRY}/e2e/templates/t1-mem:e2e"
registry_digest() {
    local repo=$1 tag=$2
    curl -fsS -I -H "Accept: application/vnd.docker.distribution.manifest.v2+json,application/vnd.oci.image.manifest.v1+json" \
        "http://${REGISTRY}/v2/${repo}/manifests/${tag}" \
        | tr -d '\r' | awk 'tolower($1)=="docker-content-digest:"{print $2}'
}
ROOTFS_DIGEST=$(registry_digest "e2e/templates/t1-rootfs" "e2e") || die "rootfs image manifest missing in registry"
MEMORY_DIGEST=$(registry_digest "e2e/templates/t1-mem" "e2e") || die "memory image manifest missing in registry"

# The subcommand's reported refs must match what the registry serves.
PIN_ROOTFS=$(awk '/^rootfs-ref:/{print $2}' "$WORK/refs.txt")
PIN_MEMORY=$(awk '/^memory-ref:/{print $2}' "$WORK/refs.txt")
[[ "$PIN_ROOTFS" == "${ROOTFS_REF}@${ROOTFS_DIGEST}" ]] \
    || die "rootfs pin mismatch: reported=$PIN_ROOTFS registry=${ROOTFS_REF}@${ROOTFS_DIGEST}"
[[ "$PIN_MEMORY" == "${MEMORY_REF}@${MEMORY_DIGEST}" ]] \
    || die "memory pin mismatch: reported=$PIN_MEMORY registry=${MEMORY_REF}@${MEMORY_DIGEST}"
log "published: rootfs=${PIN_ROOTFS} memory=${PIN_MEMORY}"

# --- registry assertions ------------------------------------------------------
registry_manifest() {
    local repo=$1 reference=$2
    curl -fsS -H "Accept: application/vnd.docker.distribution.manifest.v2+json,application/vnd.oci.image.manifest.v1+json" \
        "http://${REGISTRY}/v2/${repo}/manifests/${reference}"
}
assert_manifest() {
    local repo=$1 digest=$2 name=$3
    local manifest
    manifest=$(registry_manifest "$repo" "$digest") || die "$name: manifest fetch by digest failed"
    echo "$manifest" | jq -e '
        (.layers | length == 1)
        and (.layers[0].annotations // {} | has("containerd.io/snapshot/overlaybd/bs-digest"))
        and (.config.digest == "sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a")
    ' >/dev/null || die "$name: manifest shape mismatch (layers/annotations/empty-config):
$(echo "$manifest" | jq .)"
    log "$name manifest OK: $(echo "$manifest" | jq -c '{artifactType, layers: [.layers[].digest]}')"
}
assert_manifest "e2e/templates/t1-rootfs" "$ROOTFS_DIGEST" "rootfs"
assert_manifest "e2e/templates/t1-mem" "$MEMORY_DIGEST" "memory"

# --- byte-exactness roundtrip ---------------------------------------------------
if [[ "$SKIP_ROUNDTRIP" == 1 ]]; then
    log "--skip-roundtrip: skipping attach verification"
else
    roundtrip() {
        local repo=$1 digest=$2 fixture=$3 size=$4 sha=$5 name=$6
        local ref="${REGISTRY}/${repo}@${digest}"
        log "roundtrip $name: attach $ref"
        local attach_json
        attach_json=$(strmvolctl --plainHTTP attach --readonly true "$ref" 2>"$WORK/${name}-attach.err") \
            || { cat "$WORK/${name}-attach.err" >&2; die "$name: strmvolctl attach failed"; }
        local device volume_id
        device=$(echo "$attach_json" | jq -r '.device // .mountpoint // .mount.source // empty')
        volume_id=$(echo "$attach_json" | jq -r '.volumeID // .volume_id // empty')
        [[ -n "$device" && -b "$device" ]] || die "$name: no device in attach output: $attach_json"
        local got
        got=$(dd if="$device" bs=1M count=$((size / 1048576)) status=none | sha256sum | awk '{print $1}')
        if [[ "$got" != "$sha" ]]; then
            die "$name roundtrip digest mismatch: device=$got fixture=$sha (holes did not read as zeros?)"
        fi
        log "$name roundtrip OK (device bytes match fixture)"
        [[ -z "$volume_id" ]] || strmvolctl detach "$volume_id" >/dev/null 2>&1 || true
    }
    roundtrip "e2e/templates/t1-rootfs" "$ROOTFS_DIGEST" "$ROOTFS_FIXTURE" 1073741824 "$ROOTFS_SHA" "rootfs"
    roundtrip "e2e/templates/t1-mem" "$MEMORY_DIGEST" "$MEMORY_FIXTURE" "$MEMORY_SIZE" "$MEMORY_SHA" "memory"
fi

log "E2E passed — OCI packaging stage verified in isolation (workspace $WORK)"

#!/usr/bin/env bash
# firecracker-xfs-stateroot.sh — put the fast-sandbox StateRoot on a
# reflink-capable filesystem so per-Sandbox rootfs copies are CoW (no dirty
# pages, instant fsync) instead of paying the full dirty writeback (~2.7s per
# create on throttled hosts). Point FC_STATE_ROOT at the resulting mount when
# running scripts/firecracker-e2e.sh.
#
# Usage:
#   ./firecracker-xfs-stateroot.sh <free-disk> [size]
#       Format a free disk as xfs and mount at $MOUNT_POINT.
#       size is passed to mkfs.xfs (default: keep disk size).
#   ./firecracker-xfs-stateroot.sh --loop [size]
#       Sparse loop-backed xfs file on an existing disk (default size 64G).
#   ./firecracker-xfs-stateroot.sh --grow <size>
#       Grow an existing loop-backed xfs online (truncate + xfs_growfs).
#
# The script never touches a disk/partition that already carries a filesystem.

set -euo pipefail

MOUNT_POINT="${FC_STATE_ROOT_MOUNT:-/var/lib/fast-sandbox}"
REFLINK_LOOP_FILE="${REFLINK_LOOP_FILE:-/data/fast-sandbox.img}"

log() { printf '\033[1;34m[fc-xfs]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[fc-xfs] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

[[ "$(id -u)" -eq 0 ]] || die "must run as root"
command -v mkfs.xfs >/dev/null || die "xfsprogs not installed (yum install xfsprogs / apt install xfsprogs)"

mount_xfs() { # device_or_file mountpoint
    [[ -d "$2" ]] || mkdir -p "$2"
    mount -o noatime "$1" "$2"
}

[[ -d "$MOUNT_POINT" ]] || mkdir -p "$MOUNT_POINT"
if findmnt -no FSTYPE "$MOUNT_POINT" >/dev/null 2>&1; then
    log "$MOUNT_POINT already mounted: $(findmnt -no SOURCE,FSTYPE "$MOUNT_POINT")"
    exit 0
fi

verify_reflink() {
    a="$MOUNT_POINT/.reflink-a"
    b="$MOUNT_POINT/.reflink-b"
    printf 'probe' > "$a"
    cp --reflink=always "$a" "$b"
    rm -f "$a" "$b"
}

if [[ "${1:-}" == "--loop" ]]; then
    size="${2:-64G}"
    log "loop-backed sparse xfs: $REFLINK_LOOP_FILE (virtual ${size})"
    truncate -s "$size" "$REFLINK_LOOP_FILE"          # sparse: space is used on write
    mkfs.xfs -f "$REFLINK_LOOP_FILE"
    mount_xfs "$REFLINK_LOOP_FILE" "$MOUNT_POINT"
    echo "$REFLINK_LOOP_FILE $MOUNT_POINT xfs loop,noatime 0 0" >> /etc/fstab
elif [[ "${1:-}" == "--grow" ]]; then
    size="${2:-}"
    [[ -n "$size" ]] || die "usage: $0 --grow <size>"
    [[ -f "$REFLINK_LOOP_FILE" ]] || die "loop file missing: $REFLINK_LOOP_FILE"
    [[ -d "$MOUNT_POINT" ]] || mkdir -p "$MOUNT_POINT"
    mount_xfs "$REFLINK_LOOP_FILE" "$MOUNT_POINT"
    truncate -s "$size" "$REFLINK_LOOP_FILE"
    xfs_growfs "$MOUNT_POINT"
    log "grown to ${size}: $(df -h "$MOUNT_POINT" | tail -1)"
    exit 0
else
    DISK="${1:-}"
    [[ -n "$DISK" ]] || die "usage: $0 <free-disk> | --loop [size] | --grow <size>"
    [[ -b "$DISK" ]] || die "not a block device: $DISK"
    lsblk -no FSTYPE "$DISK" | grep -q . && die "refusing to format $DISK: it already has a filesystem"
    size="${2:-}"
    log "formatting $DISK as xfs and mounting at $MOUNT_POINT"
    if [[ -n "$size" ]]; then
        die "size argument is only valid with --loop"
    fi
    mkfs.xfs -f "$DISK"
    mount_xfs "$DISK" "$MOUNT_POINT"
    echo "$DISK $MOUNT_POINT xfs defaults,noatime 0 0" >> /etc/fstab
fi

verify_reflink && log "reflink OK on $MOUNT_POINT ($(findmnt -no FSTYPE "$MOUNT_POINT"))" \
    || die "reflink verification failed on $MOUNT_POINT"

log "done. Point the fast-sandbox StateRoot at $MOUNT_POINT"
log "ops: du -sh $MOUNT_POINT/* ; cache GC under $MOUNT_POINT/images ; fstrim -v $MOUNT_POINT ; grow with $0 --grow <size>"

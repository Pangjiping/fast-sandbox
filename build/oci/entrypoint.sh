#!/bin/sh
set -eu
export LD_LIBRARY_PATH="/opt/overlaybd/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
needs_oci=${FAST_SANDBOX_OCI_ENABLED:-true}
if [ "$needs_oci" = auto ]; then
  needs_oci=false
  case "${SANDBOX_TEMPLATE_SPEC:-}" in *'oci://'*) needs_oci=true;; esac
fi
if [ "$needs_oci" = true ]; then
  mkdir -p /var/run/overlaybd-ublk /opt/overlaybd/registry_cache
  overlaybd-ublkd --service-config /etc/overlaybd/ublkd.json --cache-dir /opt/overlaybd/registry_cache >/tmp/overlaybd-ublkd.log 2>&1 &
  daemon_pid=$!
  attempts=0
  until curl -fsS --unix-socket /var/run/overlaybd-ublk/ublkd.sock http://localhost/v1/ping >/dev/null 2>&1; do
    if ! kill -0 "$daemon_pid" 2>/dev/null; then cat /tmp/overlaybd-ublkd.log >&2; exit 1; fi
    attempts=$((attempts+1)); if [ "$attempts" -ge 100 ]; then echo 'ublkd startup timed out' >&2; exit 1; fi
    sleep 0.1
  done
fi
exec "$@"

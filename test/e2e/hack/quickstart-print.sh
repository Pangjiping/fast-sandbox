#!/usr/bin/env bash
set -Eeuo pipefail

pool_name="${1:?pool name is required}"
sandbox_name="${2:?sandbox name is required}"
data_plane="${3:-}"
config_state="${4:-existing}"
fastpath_port="${5:-9090}"
proxy_port="${6:-18080}"

echo "Terminal 1 (keep it running):"
if [[ "${fastpath_port}" == "9090" && "${proxy_port}" == "18080" ]]; then
  echo "  make quickstart-forward"
else
  echo "  QUICKSTART_FASTPATH_PORT=${fastpath_port} QUICKSTART_PROXY_PORT=${proxy_port} \\"
  echo "    make quickstart-forward"
fi
echo
case "${config_state}" in
  created)
    echo "fastctl config: created .fastctl/config.json"
    echo "The commands below will use the Quick Start endpoints automatically."
    ;;
  existing)
    echo "fastctl config: existing .fastctl/config.json was preserved"
    echo "Ensure it contains the Quick Start endpoints, edit it, or override it in terminal 2:"
    echo "  export FAST_SANDBOX_ENDPOINT=localhost:${fastpath_port}"
    echo "  export FAST_SANDBOX_PROXY_ENDPOINT=http://localhost:${proxy_port}"
    ;;
  *)
    echo "unknown Quick Start config state: ${config_state}" >&2
    exit 2
    ;;
esac
echo
echo "Terminal 2 (copy/paste in order):"
echo "  bin/fastctl run ${sandbox_name} --image docker.io/library/alpine:latest \\"
echo "    --pool ${pool_name} -- /bin/sleep 3600"
echo
echo "  kubectl wait --for=jsonpath='{.status.dataPlaneState}'=Ready \\"
echo "    sandbox/${sandbox_name} --timeout=60s"
echo
echo "  bin/fastctl get ${sandbox_name}"
echo
echo "  bin/fastctl diagnostics sandbox ${sandbox_name}"

if [[ "${data_plane}" == "execd" ]]; then
  echo
  echo "  bin/fastctl opensandbox exec ${sandbox_name} -- \\"
  echo "    sh -lc 'printf \"hello from execd\\n\" > /tmp/execd.txt && cat /tmp/execd.txt'"
  echo
  echo "  printf 'hello from host\\n' > /tmp/fast-sandbox-quickstart.txt"
  echo "  bin/fastctl opensandbox cp \\"
  echo "    /tmp/fast-sandbox-quickstart.txt ${sandbox_name}:/tmp/from-host.txt"
  echo "  bin/fastctl opensandbox files stat ${sandbox_name} /tmp/from-host.txt"
  echo "  bin/fastctl opensandbox files read ${sandbox_name} /tmp/from-host.txt"
  echo "  bin/fastctl opensandbox cp \\"
  echo "    ${sandbox_name}:/tmp/execd.txt /tmp/execd-downloaded.txt"
else
  echo
  echo "This Pool uses infraProfile=minimal; OpenSandbox exec/file commands are intentionally unavailable."
fi

echo
echo "  bin/fastctl delete ${sandbox_name}"

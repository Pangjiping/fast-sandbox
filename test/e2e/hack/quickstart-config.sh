#!/usr/bin/env bash
set -Eeuo pipefail

config_path="${1:-.fastctl/config.json}"
fastpath_port="${QUICKSTART_FASTPATH_PORT:-9090}"
proxy_port="${QUICKSTART_PROXY_PORT:-18080}"

validate_port() {
  local name="$1"
  local value="$2"
  if [[ ! "${value}" =~ ^[0-9]+$ ]] ||
    ((10#${value} < 1 || 10#${value} > 65535)); then
    echo "${name} must be an integer between 1 and 65535, got ${value}" >&2
    exit 2
  fi
}

validate_port QUICKSTART_FASTPATH_PORT "${fastpath_port}"
validate_port QUICKSTART_PROXY_PORT "${proxy_port}"

if [[ -e "${config_path}" || -L "${config_path}" ]]; then
  printf 'existing\n'
  exit 0
fi

config_dir="$(dirname -- "${config_path}")"
mkdir -p "${config_dir}"

umask 077
temporary_path="$(mktemp "${config_path}.tmp.XXXXXX")"
cleanup() {
  rm -f "${temporary_path}"
}
trap cleanup EXIT

printf '{\n  "endpoint": "localhost:%s",\n  "proxy-endpoint": "http://localhost:%s"\n}\n' \
  "${fastpath_port}" "${proxy_port}" >"${temporary_path}"

# Linking a fully written temporary file creates the final path atomically and
# fails instead of overwriting a file created concurrently.
if ln "${temporary_path}" "${config_path}" 2>/dev/null; then
  printf 'created\n'
  exit 0
fi
if [[ -e "${config_path}" || -L "${config_path}" ]]; then
  printf 'existing\n'
  exit 0
fi

echo "failed to create ${config_path}" >&2
exit 1

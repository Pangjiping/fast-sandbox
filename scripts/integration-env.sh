#!/usr/bin/env bash
# integration-env.sh — Kind single-host full-chain integration environment.
#
# Builds the fast-sandbox Firecracker on-demand loading chain on a bare-metal
# KVM host: SandboxTemplate golden-image build (builder Pod in-cluster) →
# publish MinIO → node runtime-agent (DaemonSet) → fastlet sandbox restore →
# execd /ping delivery verification.
#
# See docs/guides/firecracker-integration-environment.md and
# docs/design/firecracker-integration-environment-plan.md (this script is
# the "one-click" task 10 of the plan).
#
# Usage:
#   ./scripts/integration-env.sh up            # full environment + chain
#   ./scripts/integration-env.sh status        # component/template/pool health
#   ./scripts/integration-env.sh verify        # sandbox create + execd /ping
#   ./scripts/integration-env.sh down          # teardown, host left clean
#   ./scripts/integration-env.sh --cleanup     # down after an interrupted run
#   ./scripts/integration-env.sh up --auto-clean   # down automatically on failure
#
# Environment overrides (all optional):
#   WORK, KIND_CLUSTER, MINIO_PORT, MINIO_AK, MINIO_SK, MINIO_IMAGE,
#   MINIO_ENDPOINT, IMAGE_<NAME> (image tags), FC_VERSION, SBX_IMAGE
#
# Every task logs to $WORK/logs/; failures dump component logs to
# logs/failure-<task>-<ts>.txt before exiting (never silently).
#
# Output: numbered milestone banners (==> [n] ...), per-stage durations,
# a stage-timings table and an environment summary after up/verify, plus the
# per-sandbox golden-restore breakdown (total/acquire/rootfs/launch/boot)
# extracted from the fastlet driver logs during verify.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${WORK:-$PWD/.integration-env}"
LOGS_DIR="$WORK/logs"
GEN_DIR="$REPO_ROOT/.integration-env-gen"

KIND_CLUSTER="${KIND_CLUSTER:-firecracker}"
KIND_CONFIG="$REPO_ROOT/config/dev/kind-firecracker.yaml"
NS="fast-sandbox-system"

MINIO_IMAGE="${MINIO_IMAGE:-minio/minio:latest}"
MINIO_PORT="${MINIO_PORT:-9000}"
MINIO_AK="${MINIO_AK:-integration-env}"
MINIO_SK="${MINIO_SK:-integration-env-secret}"
MINIO_BUCKET="sandbox-images"
MINIO_CONTAINER="integration-env-minio"
MINIO_DATA="$WORK/minio-data"
MINIO_ENDPOINT="${MINIO_ENDPOINT:-}"   # auto-derived from the Kind network
STORE_ROOT="s3://$MINIO_BUCKET/publish"

FC_VERSION="${FC_VERSION:-v1.16.1}"
# The template source image doubles as the artifact key (the builder pulls
# it from a registry; index/digest16 are keyed by sha256 of this reference).
SBX_IMAGE="${SBX_IMAGE:-alpine:3.19}"
EXECD="${EXECD:-opensandbox/execd:1.1.0}"
ROOTFS_SIZE="${ROOTFS_SIZE:-2Gi}"
SBX_TEMPLATE="ai-office-sandbox"
SBX_POOL="firecracker-pool"
SBX_SANDBOX="sandbox-firecracker"

# Node labels. The KVM label key is hardcoded by the SandboxTemplate
# reconciler (sandbox.fast.io/kvm); the firecracker label selects installer/
# agent/fastlet placement.
KVM_NODE_LABEL="sandbox.fast.io/kvm"
FC_NODE_LABEL="fast-sandbox.io/firecracker-node"

INOTIFY_VALUE="${INOTIFY_VALUE:-8192}"
SYSCTL_BACKUP="$WORK/sysctl-backup"

# Image tags (env-overridable per component).
IMG_CONTROLLER="${IMAGE_CONTROLLER:-fast-sandbox/controller:dev}"
IMG_FASTLET="${IMAGE_FASTLET:-fast-sandbox/fastlet:dev}"
IMG_FASTLET_PROXY="${IMAGE_FASTLET_PROXY:-fast-sandbox/fastlet-proxy:dev}"
IMG_SANDBOX_PROXY="${IMAGE_SANDBOX_PROXY:-fast-sandbox/sandbox-proxy:dev}"
IMG_JANITOR="${IMAGE_JANITOR:-fast-sandbox/janitor:dev}"
IMG_BUILDER="${IMAGE_BUILDER:-fast-sandbox/sandboxtemplate-builder:dev}"
IMG_AGENT="${IMAGE_AGENT:-fast-sandbox/firecracker-runtime-agent:dev}"

AUTO_CLEAN=0
ACTION=""

log() { printf '\033[1;34m[firecracker-integration]\033[0m %s\n' "$*" | tee -a "$WORK/run.log"; }
die() { printf '\033[1;31m[firecracker-integration] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }
pass() { printf '\033[1;32m[firecracker-integration] PASS\033[0m %s\n' "$*" | tee -a "$WORK/run.log"; }
fail() { printf '\033[1;31m[firecracker-integration] FAIL\033[0m %s\n' "$*" >&2; exit 1; }
# highlight() marks a key milestone in the output (bold cyan, not logged).
highlight() { printf '\033[1;36m%s\033[0m\n' "$*"; }

# --- milestone + timing ------------------------------------------------------
# run_stage wraps every task with a numbered milestone banner and records the
# elapsed time; up/verify print a stage-summary table at the end.
now_ms() { date +%s%N; }   # GNU date (Linux); the script targets the test node
ms2s() { awk -v ms="$1" 'BEGIN { printf "%.1f", ms / 1000 }'; }

declare -a STAGE_ORDER=()     # stage names in insertion order
declare -a STAGE_MS_LIST=()   # parallel durations in ms
STAGE_CUR=""
STAGE_START_NS=0
STAGE_N=0

stage_begin() { # description
	STAGE_N=$((STAGE_N + 1))
	STAGE_CUR="$1"
	STAGE_START_NS="$(now_ms)"
	printf '\n\033[1;36m==> [%d] %s\033[0m\n' "$STAGE_N" "$1"
}

stage_done() { # [status-detail]
	local ms
	ms=$(( ($(now_ms) - STAGE_START_NS) / 1000000 ))
	STAGE_ORDER+=("$STAGE_CUR")
	STAGE_MS_LIST+=("$ms")
	printf '\033[1;32m    OK in %ss\033[0m %s\n' "$(ms2s "$ms")" "${1:-}"
}

run_stage() { # description function [args...]
	local description="$1" func="$2"
	shift 2
	stage_begin "$description"
	"$func" "$@"
	stage_done
}

stage_summary() {
	local index name ms total=0
	highlight "== stage timings =="
	printf '  \033[1m%-40s %10s\033[0m\n' "stage" "duration"
	for index in "${!STAGE_ORDER[@]}"; do
		name="${STAGE_ORDER[$index]}"
		ms="${STAGE_MS_LIST[$index]}"
		total=$((total + ms))
		printf '  %-40s %9ss\n' "$name" "$(ms2s "$ms")"
	done
	printf '  \033[1m%-40s %9ss\033[0m\n' "TOTAL" "$(ms2s "$total")"
}

wait_for() { # description attempts command [args...]
	local description="$1" attempts="$2" attempt=0
	shift 2
	while ! "$@" >/dev/null 2>&1; do
		attempt=$((attempt + 1))
		if [[ "$attempt" -ge "$attempts" ]]; then
			fail "$description (after $attempts attempts)"
		fi
		sleep 2
	done
	pass "$description"
}

# wait_until polls at 10ms granularity up to a millisecond deadline — used
# for the delivery-baseline waits (sandbox Ready, execd /ping), where the
# 2s polling of wait_for would drown the measurement. Requires GNU sleep
# (fractional seconds; the script targets the Linux test node).
wait_until() { # description timeout-ms command [args...]
	local description="$1" timeout_ms="$2"
	shift 2
	local deadline=$(( $(now_ms) + timeout_ms ))
	while ! "$@" >/dev/null 2>&1; do
		if [[ "$(now_ms)" -ge "$deadline" ]]; then
			fail "$description (after ${timeout_ms}ms)"
		fi
		sleep 0.01
	done
	pass "$description"
}

kubectl_get() { kubectl -n "$NS" get "$1" -o jsonpath="$2"; }

# --- condition helpers (plain functions, safe for wait_for) ------------------
template_succeeded() {
	[[ "$(kubectl_get "sandboxtemplate/$SBX_TEMPLATE" '{.status.phase}')" == "Succeeded" ]]
}

template_failed() {
	[[ "$(kubectl_get "sandboxtemplate/$SBX_TEMPLATE" '{.status.phase}')" == "Failed" ]]
}

fastlet_pod_ready() {
	local pod
	pod="$(kubectl -n "$NS" get pods -l app=sandbox-fastlet -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)"
	[[ -n "$pod" ]] && kubectl -n "$NS" get pod "$pod" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' | grep -q True
}

warm_images_ready() {
	kubectl -n "$NS" get sandboxpool "$SBX_POOL" -o jsonpath='{.status.warmImages[*].cachedFastlets}' 2>/dev/null | grep -qv '^0*$'
}

agent_pod() {
	kubectl -n "$NS" get pods -l component=firecracker-runtime-agent -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

agent_leases_drained() {
	local pod uid
	pod="$(agent_pod)"
	uid="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.metadata.uid}')"
	kubectl exec -n "$NS" "$pod" -- sh -c \
		"curl -fsS --unix-socket /run/fast-sandbox/firecracker/runtime.sock -H 'Content-Type: application/json' -d '{\"podUid\":\"$uid\",\"namespace\":\"$NS\"}' http://firecracker-agent/v1/list-leases" \
		| grep -q '"leases":\[\]'
}

jails_cleaned() {
	local node
	node="$(kind_node)"
	# removeJailRoot removes <state>/jails/<exec>/<id> per sandbox; the
	# exec-level dir (jails/firecracker) is permanent and must be empty.
	[[ -z "$(docker exec "$node" sh -c 'ls -A /var/lib/fast-sandbox/firecracker/jails/firecracker 2>/dev/null' || true)" ]]
}

# --- failure dump --------------------------------------------------------------
on_error() { # task
	local task="$1"
	if [[ "$AUTO_CLEAN" == 1 ]]; then
		log "up failed at $task; --auto-clean: running down"
		down >/dev/null 2>&1 || true
	fi
	failure_dump "$task"
	printf '\033[1;31m[firecracker-integration] FAILED at %s; dump: %s\033[0m\n' \
		"$task" "$LOGS_DIR/failure-$task-*.txt" >&2
}

failure_dump() { # task
	local task="$1" dump="$LOGS_DIR/failure-$task-$(date +%s).txt"
	mkdir -p "$LOGS_DIR"
	{
		echo "=== integration-env failure: $task ($(date -u +%FT%TZ)) ==="
		env | grep -E '^(MINIO|KIND|FC_|SBX|IMG_|EXECD|WORK)=' || true
		echo "--- kind nodes ---"
		kind get nodes --name "$KIND_CLUSTER" 2>&1 || true
		echo "--- kind-create.log (tail) ---"
		tail -40 "$LOGS_DIR/kind-create.log" 2>&1 || true
		echo "--- pods ---"
		kubectl get pods -n "$NS" -o wide 2>&1 || true
		echo "--- controller logs (tail) ---"
		kubectl logs -n "$NS" deploy/fast-sandbox-controller --tail=80 2>&1 || true
		echo "--- agent logs (tail) ---"
		kubectl logs -n "$NS" daemonset/firecracker-runtime-agent --tail=80 2>&1 || true
		echo "--- installer logs (tail) ---"
		kubectl logs -n "$NS" daemonset/firecracker-runtime-installer --all-containers --tail=80 2>&1 || true
		echo "--- fastlet logs (tail) ---"
		kubectl logs -n "$NS" -l app=sandbox-fastlet --tail=80 2>&1 || true
		echo "--- builder pods + logs (tail) ---"
		kubectl get pods -n "$NS" -l sandbox.fast.io/sandboxtemplate --show-labels 2>&1 || true
		kubectl logs -n "$NS" -l sandbox.fast.io/sandboxtemplate --tail=80 2>&1 || true
		echo "--- template ---"
		kubectl get sandboxtemplate -n "$NS" -o yaml 2>&1 || true
		echo "--- pool ---"
		kubectl get sandboxpool -n "$NS" "$SBX_POOL" -o yaml 2>&1 || true
		echo "--- minio docker logs (tail) ---"
		docker logs "$MINIO_CONTAINER" --tail=80 2>&1 || true
		echo "--- node kvm ---"
		local node
		node="$(kind get nodes --name "$KIND_CLUSTER" 2>/dev/null | head -1 || true)"
		[[ -n "$node" ]] && docker exec "$node" sh -c 'ls -l /dev/kvm; grep -c vmx /proc/cpuinfo' 2>&1 || true
	} > "$dump" 2>&1 || true
	log "failure dump: $dump"
}

# --- helpers ---------------------------------------------------------------------
kind_node() { kind get nodes --name "$KIND_CLUSTER" 2>/dev/null | head -1; }

kind_network() { # name of the docker network the node container is on
	local node
	node="$(kind_node)" || return 1
	docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$node" | tr ' ' '\n' | grep -v '^$' | head -1
}

mc() { docker run --rm --network host -v "$WORK/mc-config:/root/.mc" minio/mc "$@"; }

# gen-registry: compiled registryconfig JSON for one S3 credential (same
# pattern as scripts/firecracker-chain-e2e.sh).
gen_registry() { # host username password endpoint > registry.json
	mkdir -p "$GEN_DIR"
	cat > "$GEN_DIR/gen-registry.go" <<'EOF'
package main

import (
	"fmt"
	"os"

	"fast-sandbox/internal/registryconfig"
)

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: gen-registry <host> <username> <password> <endpoint>")
		os.Exit(1)
	}
	compiled, err := registryconfig.NewCompiled([]registryconfig.Credential{{
		Host: os.Args[1], Username: os.Args[2], Password: os.Args[3], Endpoint: os.Args[4],
	}})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	payload, err := compiled.Marshal()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Stdout.Write(payload)
}
EOF
	(cd "$REPO_ROOT" && GOTOOLCHAIN=local go run .integration-env-gen/gen-registry.go "$@")
}

# --- task 1: preflight + sysctl ----------------------------------------------------
# Missing tooling is installed automatically (kind/kubectl from official
# release binaries, jq from the package manager). Set SKIP_TOOL_INSTALL=1 to
# only verify and point at the manual install steps instead.
KIND_VERSION="${KIND_VERSION:-v0.24.0}"
KUBECTL_VERSION="${KUBECTL_VERSION:-v1.31.0}"
SKIP_TOOL_INSTALL="${SKIP_TOOL_INSTALL:-0}"
SKIP_LEFTOVER_CLEAN="${SKIP_LEFTOVER_CLEAN:-0}"
KIND_RETAIN="${KIND_RETAIN:-0}"

sudo_() { if [[ "$(id -u)" == 0 ]]; then "$@"; else sudo "$@"; fi; }

install_release_binary() { # name version url
	local name="$1" version="$2" url="$3" tmp
	log "installing $name $version -> /usr/local/bin/$name"
	tmp="$(mktemp -d)"
	curl -fL --retry 3 -o "$tmp/$name" "$url" \
		|| die "download $name failed ($url); install it manually or retry"
	sudo_ install -m 0755 "$tmp/$name" "/usr/local/bin/$name"
	rm -rf "$tmp"
}

ensure_tool() { # name
	local name="$1"
	if command -v "$name" >/dev/null 2>&1; then
		return 0
	fi
	if [[ "$SKIP_TOOL_INSTALL" == 1 ]]; then
		die "$name is required (SKIP_TOOL_INSTALL=1: install it manually, see logs/environment.txt)"
	fi
	case "$name" in
		kind)
			install_release_binary kind "$KIND_VERSION" \
				"https://github.com/kubernetes-sigs/kind/releases/download/$KIND_VERSION/kind-linux-amd64"
			;;
		kubectl)
			install_release_binary kubectl "${KUBECTL_VERSION#v}" \
				"https://dl.k8s.io/release/$KUBECTL_VERSION/bin/linux/amd64/kubectl"
			;;
		jq)
			log "installing jq via package manager"
			if command -v apt-get >/dev/null 2>&1; then
				sudo_ apt-get install -y jq >/dev/null
			elif command -v yum >/dev/null 2>&1; then
				sudo_ yum install -y jq >/dev/null
			elif command -v apk >/dev/null 2>&1; then
				sudo_ apk add --no-cache jq >/dev/null
			else
				die "no supported package manager to install jq; install it manually"
			fi
			;;
		*)
			die "$name is required; install it manually"
			;;
	esac
	command -v "$name" >/dev/null 2>&1 || die "$name installation failed"
}

preflight() {
	command -v docker >/dev/null || die "docker is required"
	command -v go >/dev/null || die "go is required (>=1.25, used by make images and gen-registry)"
	ensure_tool kind
	ensure_tool kubectl
	ensure_tool jq
	docker info >/dev/null 2>&1 || die "docker daemon is not reachable"
	local cgdriver cgver
	cgdriver="$(docker info --format '{{.CgroupDriver}}' 2>/dev/null || true)"
	cgver="$(docker info --format '{{.CgroupVersion}}' 2>/dev/null || true)"
	log "docker cgroup driver=$cgdriver version=$cgver"
	# kind requires cgroup v2: on v1 hosts kubelet fails to create the
	# kubepods cgroup ("failed to initialize top level QOS containers")
	# regardless of the docker cgroup driver.
	if [[ "$cgver" == "1" ]]; then
		die "docker cgroup Version is 1; kind requires cgroup v2. Enable it with the kernel cmdline 'systemd.unified_cgroup_hierarchy=1' (update-grub / grub2-mkconfig) and reboot, then verify 'docker info' shows Cgroup Version: 2"
	fi
	[[ -e /dev/kvm ]] || die "/dev/kvm is missing on this host (KVM required)"
	docker pull -q "$MINIO_IMAGE" >/dev/null
	docker pull -q minio/mc >/dev/null
	pass "preflight"
}

sysctl_set() {
	local current
	current="$(sysctl -n fs.inotify.max_user_instances)"
	[[ "$current" -ge "$INOTIFY_VALUE" ]] && return 0
	echo "$current" > "$SYSCTL_BACKUP"
	log "sysctl fs.inotify.max_user_instances: $current -> $INOTIFY_VALUE"
	sudo sysctl -w fs.inotify.max_user_instances="$INOTIFY_VALUE" >/dev/null
}

sysctl_restore() {
	[[ -f "$SYSCTL_BACKUP" ]] || return 0
	local previous
	previous="$(cat "$SYSCTL_BACKUP")"
	log "restoring fs.inotify.max_user_instances -> $previous"
	sudo sysctl -w fs.inotify.max_user_instances="$previous" >/dev/null || true
	rm -f "$SYSCTL_BACKUP"
}

# --- XFS StateRoot (reflink CoW per-sandbox rootfs) ---------------------------
# The per-instance rootfs copy is a full copy on ext4 (~2.5s per sandbox,
# images.go copyReflinkOrCopy). Mounting an XFS loop image at
# /var/lib/fast-sandbox (passed into the kind node, the runtime plan's
# StateRoot lives under it) turns the copy into a CoW reflink (~ms).
# Disable with XFS_STATEROOT=0; the plain directory then works as before.
XFS_STATEROOT="${XFS_STATEROOT:-1}"
XFS_LOOP_FILE="${XFS_LOOP_FILE:-$WORK/fast-sandbox.img}"
XFS_SIZE="${XFS_SIZE:-16G}"
XFS_MOUNT_POINT="${XFS_MOUNT_POINT:-/var/lib/fast-sandbox}"

ensure_xfsprogs() {
	command -v mkfs.xfs >/dev/null 2>&1 && return 0
	log "installing xfsprogs (mkfs.xfs)"
	if command -v apt-get >/dev/null 2>&1; then
		sudo_ apt-get install -y xfsprogs >/dev/null
	elif command -v yum >/dev/null 2>&1; then
		sudo_ yum install -y xfsprogs >/dev/null
	else
		die "xfsprogs not installed and no supported package manager"
	fi
}

stateroot_xfs_up() {
	[[ "$XFS_STATEROOT" == 1 ]] || {
		log "XFS StateRoot disabled (XFS_STATEROOT=0); per-sandbox rootfs pays a full copy"
		return 0
	}
	if findmnt -no FSTYPE "$XFS_MOUNT_POINT" 2>/dev/null | grep -qx xfs; then
		log "XFS StateRoot already mounted at $XFS_MOUNT_POINT: $(findmnt -no SOURCE,FSTYPE "$XFS_MOUNT_POINT")"
		pass "XFS StateRoot ready (reflink CoW rootfs)"
		return 0
	fi
	ensure_xfsprogs
	if [[ ! -f "$XFS_LOOP_FILE" ]]; then
		log "creating sparse XFS image $XFS_LOOP_FILE (virtual $XFS_SIZE)"
		truncate -s "$XFS_SIZE" "$XFS_LOOP_FILE"
		sudo_ mkfs.xfs -f "$XFS_LOOP_FILE" >/dev/null 2>&1 || die "mkfs.xfs failed on $XFS_LOOP_FILE"
	fi
	sudo_ mkdir -p "$XFS_MOUNT_POINT"
	sudo_ mount -o noatime "$XFS_LOOP_FILE" "$XFS_MOUNT_POINT" \
		|| die "mount $XFS_LOOP_FILE at $XFS_MOUNT_POINT failed (loop support? try XFS_STATEROOT=0)"
	# probe: reflink must actually work on the resulting filesystem.
	local a b
	a="$XFS_MOUNT_POINT/.reflink-a"
	b="$XFS_MOUNT_POINT/.reflink-b"
	printf 'probe' > "$a"
	if sudo_ cp --reflink=always "$a" "$b"; then
		sudo_ rm -f "$a" "$b"
		pass "XFS StateRoot ready (reflink CoW rootfs)"
	else
		sudo_ rm -f "$a" "$b"
		die "reflink probe failed on $XFS_MOUNT_POINT (CoW rootfs would not work)"
	fi
}

stateroot_xfs_down() {
	[[ "$XFS_STATEROOT" == 1 ]] || return 0
	if findmnt -no SOURCE "$XFS_MOUNT_POINT" 2>/dev/null | grep -q "$(basename "$XFS_LOOP_FILE")"; then
		log "unmounting XFS StateRoot $XFS_MOUNT_POINT"
		sudo_ umount "$XFS_MOUNT_POINT"
	fi
	rm -f "$XFS_LOOP_FILE"
}

build_images() {
	log "building images (controller/fastlet/fastlet-proxy/sandbox-proxy/janitor/agent)"
	(cd "$REPO_ROOT" && make images COMPONENT=controller >/dev/null)
	(cd "$REPO_ROOT" && make images COMPONENT=fastlet >/dev/null)
	(cd "$REPO_ROOT" && make images COMPONENT=fastlet-proxy >/dev/null)
	(cd "$REPO_ROOT" && make images COMPONENT=sandbox-proxy >/dev/null)
	(cd "$REPO_ROOT" && make images COMPONENT=janitor >/dev/null)
	(cd "$REPO_ROOT" && make images COMPONENT=firecracker-runtime-agent >/dev/null)
	log "building sandboxtemplate-builder image"
	docker build --quiet -t "$IMG_BUILDER" \
		-f "$REPO_ROOT/build/Dockerfile.sandboxtemplate-builder" "$REPO_ROOT" >/dev/null
	log "building fastctl (host CLI)"
	mkdir -p "$WORK/bin"
	(cd "$REPO_ROOT" && GOTOOLCHAIN=local go build -o "$WORK/bin/fastctl" ./cmd/fastctl)
	log "building gen-endpoint (proxy route helper)"
	write_gen_endpoint_source
	(cd "$REPO_ROOT" && GOTOOLCHAIN=local go build -o "$WORK/bin/gen-endpoint" .integration-env-gen/gen-endpoint.go)
	pass "images built"
}

write_gen_endpoint_source() {
	mkdir -p "$GEN_DIR" "$WORK/bin"
	cat > "$GEN_DIR/gen-endpoint.go" <<'EOF'
// Command gen-endpoint resolves the central-proxy route of a Sandbox port.
//
// Daemon mode (--daemon <socket> <grpc-endpoint>) keeps the FastPath gRPC
// connection warm and serves resolves over a unix-socket HTTP endpoint, so
// latency measurement does not pay process spawn / connection setup per
// probe:
//
//	GET http://localhost/resolve?ns=<ns>&name=<name>&port=<port>
//	-> "<proxy endpoint path>\t<route credential>" (text/plain)
//
// One-shot mode prints the same line for the given arguments.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	fastpathv2 "fast-sandbox/api/proto/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func resolve(client fastpathv2.FastPathServiceClient, ns, name string, port uint32) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := client.ResolveEndpoint(ctx, &fastpathv2.ResolveEndpointRequest{
		Sandbox: &fastpathv2.SandboxReference{NamespacedName: &fastpathv2.NamespacedName{Namespace: ns, Name: name}},
		Target:  &fastpathv2.EndpointTarget{Target: &fastpathv2.EndpointTarget_Port{Port: port}},
		// DIRECT_FASTLET_PROXY: the endpoint is the assigned fastlet's
		// fastlet-proxy (:5780), resolved from the durable assignment
		// ANNOTATION — no dependency on the eventually-consistent
		// status.placement projection or the central sandbox-proxy.
		AccessMode: fastpathv2.EndpointAccessMode_DIRECT_FASTLET_PROXY,
	})
	if err != nil {
		return "", "", err
	}
	credential := ""
	for header, value := range resp.GetRequiredHeaders() {
		if header == "X-Fast-Sandbox-Route-Credential" {
			credential = value
		}
	}
	return resp.GetProxyEndpoint(), credential, nil
}

func dial(endpoint string) *grpc.ClientConn {
	conn, err := grpc.Dial(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return conn
}

func daemon(socketPath, endpoint string) {
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Chmod(socketPath, 0o660)
	conn := dial(endpoint)
	defer conn.Close()
	client := fastpathv2.NewFastPathServiceClient(conn)
	// Warm the connection before any measurement starts.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, _ = client.ListSandboxes(ctx, &fastpathv2.ListSandboxesRequest{})
	cancel()

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/resolve", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		ns, name, portValue := query.Get("ns"), query.Get("name"), query.Get("port")
		port, err := strconv.ParseUint(portValue, 10, 32)
		if err != nil || ns == "" || name == "" {
			http.Error(w, "ns/name/port are required", http.StatusBadRequest)
			return
		}
		// The third output field is the ResolveEndpoint wall time, so the
		// probe can split control-plane resolve cost from the curl data
		// path without relying on curl header support.
		started := time.Now()
		path, credential, err := resolve(client, ns, name, uint32(port))
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, "%s\t%s\t%dms\n", path, credential, time.Since(started).Milliseconds())
	})
	server := &http.Server{Handler: mux}
	if err := server.Serve(listener); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func main() {
	if len(os.Args) == 4 && os.Args[1] == "--daemon" {
		daemon(os.Args[2], os.Args[3])
		return
	}
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: gen-endpoint <grpc-endpoint> <namespace> <sandbox-name> <port> | gen-endpoint --daemon <socket> <grpc-endpoint>")
		os.Exit(1)
	}
	conn := dial(os.Args[1])
	defer conn.Close()
	port, err := strconv.ParseUint(os.Args[4], 10, 32)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	path, credential, err := resolve(fastpathv2.NewFastPathServiceClient(conn), os.Args[2], os.Args[3], uint32(port))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%s\t%s\n", path, credential)
}
EOF
}

# --- task 2: kind cluster -------------------------------------------------------------
# KIND_NODE_IMAGE overrides the kindest/node image (e.g. a local mirror when
# registry.k8s.io is unreachable); the script pulls it explicitly first so a
# slow/blocked pull fails loudly instead of inside kind create.
# KIND_RETAIN=1 keeps the failed node container for diagnosis (kind --retain).
kind_up() {
	local create_args=()
	[[ "$KIND_RETAIN" == 1 ]] && create_args+=(--retain)
	if [[ -n "$(kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" || true)" ]]; then
		log "cluster $KIND_CLUSTER already exists; reusing (run down first for a clean rebuild)"
	else
		if [[ -n "${KIND_NODE_IMAGE:-}" ]]; then
			log "pulling kind node image $KIND_NODE_IMAGE (this can take minutes)"
			docker pull -q "$KIND_NODE_IMAGE" || die "kind node image pull failed (KIND_NODE_IMAGE=$KIND_NODE_IMAGE)"
			log "creating cluster with node image $KIND_NODE_IMAGE"
			kind create cluster --name "$KIND_CLUSTER" --image "$KIND_NODE_IMAGE" \
				"${create_args[@]}" --config "$KIND_CONFIG" > "$LOGS_DIR/kind-create.log" 2>&1 \
				|| fail "kind create failed (full log: $LOGS_DIR/kind-create.log)"
		else
			log "creating cluster (pulling kindest/node may take minutes; set KIND_NODE_IMAGE to a mirror if it fails)"
			kind create cluster --name "$KIND_CLUSTER" --config "$KIND_CONFIG" \
				"${create_args[@]}" > "$LOGS_DIR/kind-create.log" 2>&1 \
				|| fail "kind create failed (full log: $LOGS_DIR/kind-create.log)"
		fi
		pass "kind cluster created"
	fi
	local node
	node="$(kind_node)"
	docker exec "$node" sh -c 'test -e /dev/kvm' || die "KVM not visible inside the kind node container"
	kubectl label node "$node" "$KVM_NODE_LABEL=true" --overwrite >/dev/null
	kubectl label node "$node" "$FC_NODE_LABEL=true" --overwrite >/dev/null
	pass "kind cluster ready (kvm passthrough + labels)"
}

# --- task 3: MinIO + credentials -------------------------------------------------------
minio_up() {
	docker rm -f "$MINIO_CONTAINER" >/dev/null 2>&1 || true
	rm -rf "$MINIO_DATA"
	mkdir -p "$MINIO_DATA"
	local net
	net="$(kind_network)"
	# Joining the kind network avoids docker-proxy/hairpin reachability
	# issues: pods and the node container talk to the container IP directly,
	# while 127.0.0.1 publishing keeps host-side mc/curl working.
	docker run -d --name "$MINIO_CONTAINER" --network "$net" \
		-p 127.0.0.1:"$MINIO_PORT":9000 -p 127.0.0.1:9001:9001 \
		-e MINIO_ROOT_USER="$MINIO_AK" -e MINIO_ROOT_PASSWORD="$MINIO_SK" \
		-v "$MINIO_DATA:/data" \
		"$MINIO_IMAGE" server /data --console-address ":9001" >/dev/null
	for attempt in $(seq 1 30); do
		if curl -fsS "http://127.0.0.1:$MINIO_PORT/minio/health/live" >/dev/null 2>&1; then break; fi
		sleep 1
		[[ "$attempt" == 30 ]] && die "MinIO did not become healthy"
	done
	# /health/live answers before the S3 API finishes initializing; retry the
	# mc alias until the server really accepts credentials.
	for attempt in $(seq 1 30); do
		if mc alias set chain "http://127.0.0.1:$MINIO_PORT" "$MINIO_AK" "$MINIO_SK" >/dev/null 2>&1; then break; fi
		sleep 1
		[[ "$attempt" == 30 ]] && die "MinIO S3 API not initialized (mc alias failed)"
	done
	mc mb "chain/$MINIO_BUCKET" >/dev/null
	pass "MinIO up (bucket=$MINIO_BUCKET)"
}

# resolve the endpoint pods inside the kind node container use for MinIO:
# the container's own IP on the kind network (container-to-container on the
# docker bridge needs no port publishing). Override with MINIO_ENDPOINT.
resolve_minio_endpoint() {
	if [[ -n "$MINIO_ENDPOINT" ]]; then
		log "task 3: MinIO endpoint (env): $MINIO_ENDPOINT"
	else
		local net ips ip
		net="$(kind_network)"
		ips="$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{$v.IPAddress}} {{end}}' "$MINIO_CONTAINER")"
		ip="$(printf '%s' "$ips" | tr ' ' '\n' | grep -A1 -x "^$net$" | tail -1)"
		[[ -n "$ip" ]] || die "could not find the MinIO IP on network $net (inspect: $ips)"
		MINIO_ENDPOINT="http://$ip:$MINIO_PORT"
		log "task 3: MinIO endpoint (kind network IP): $MINIO_ENDPOINT"
	fi
	local net
	net="$(kind_network)"
	docker run --rm --network "$net" minio/mc alias set chain \
		"$MINIO_ENDPOINT" "$MINIO_AK" "$MINIO_SK" >/dev/null 2>&1 \
		|| die "MinIO unreachable from the kind network at $MINIO_ENDPOINT (override MINIO_ENDPOINT; check host firewalld/iptables if the container IP also fails)"
	pass "MinIO reachable from the kind network"
}

credentials_up() {
	# The secrets land in the platform namespace; make sure it exists even
	# when controller_up has not run yet (e.g. resume after a partial up).
	kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
	local host
	host="${MINIO_ENDPOINT#http://}"
	host="${host#https://}"
	# Publish credentials: SecretKeyRef'd by the builder Pod (same namespace
	# as the template).
	kubectl -n "$NS" create secret generic sandbox-oss-credentials \
		--from-literal=accessKeyId="$MINIO_AK" \
		--from-literal=secretAccessKey="$MINIO_SK" \
		--from-literal=endpoint="$MINIO_ENDPOINT" \
		--from-literal=region=us-east-1 \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	# Pull credentials for the runtime-agent (compiled registryconfig).
	gen_registry "$host" "$MINIO_AK" "$MINIO_SK" "$MINIO_ENDPOINT" > "$WORK/agent-registry.json"
	jq -e . "$WORK/agent-registry.json" >/dev/null || die "generated agent registry.json is invalid"
	kubectl -n "$NS" create secret generic fast-sandbox-agent-registry \
		--from-file=registry.json="$WORK/agent-registry.json" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	# Agent endpoint override (connection address for SigV4 signing).
	kubectl -n "$NS" create configmap fast-sandbox-agent-config \
		--from-literal=artifact-endpoint="$MINIO_ENDPOINT" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	# Pull credentials for the fastlet (pool-compiled registry).
	kubectl -n "$NS" create secret docker-registry registry-minio \
		--docker-server="$host" --docker-username="$MINIO_AK" --docker-password="$MINIO_SK" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	kubectl -n "$NS" create configmap fast-sandbox-registry \
		--from-literal="registries.yaml=registries:
  - host: $host
    secretRef:
      name: registry-minio
" \
		--dry-run=client -o yaml | kubectl apply -f - >/dev/null
	pass "credentials written"
}

# --- task 4: CRDs + controller ----------------------------------------------------------
controller_up() {
	kubectl apply -k "$REPO_ROOT/config/crd" >/dev/null
	kubectl apply -k "$REPO_ROOT/config/all-in-one" >/dev/null
	for image in "$IMG_CONTROLLER" "$IMG_FASTLET" "$IMG_FASTLET_PROXY" "$IMG_SANDBOX_PROXY" "$IMG_JANITOR" "$IMG_BUILDER" "$IMG_AGENT"; do
		kind load docker-image "$image" --name "$KIND_CLUSTER" >/dev/null
	done
	wait_for "controller deployment ready" 120 \
		kubectl -n "$NS" rollout status deploy/fast-sandbox-controller --timeout=10s
	for crd in sandboxpools sandboxtemplates sandboxes; do
		kubectl get crd "$crd.sandbox.fast.io" >/dev/null 2>&1 || die "CRD $crd missing"
	done
	pass "CRDs + controller ready"
}

# --- task 5: node assets + runtime environment -------------------------------------------
installer_up() {
	kubectl apply -f "$REPO_ROOT/config/runtime-installers/firecracker.yaml" >/dev/null
	wait_for "firecracker installer ready" 60 \
		kubectl -n "$NS" rollout status daemonset/firecracker-runtime-installer --timeout=15s
	pass "firecracker/jailer/kernel installed on the node"
}

# --- task 6: agent DaemonSet ----------------------------------------------------------------
agent_up() {
	kubectl apply -f "$REPO_ROOT/config/dev/agent-daemonset.yaml" >/dev/null
	wait_for "runtime-agent DaemonSet ready" 120 \
		kubectl -n "$NS" rollout status daemonset/firecracker-runtime-agent --timeout=10s
	pass "runtime-agent healthy (UDS /v1/health)"
}

# --- task 7: SandboxTemplate build -----------------------------------------------------------
# wait_succeeded waits for a condition while bailing out early (with a log
# dump) as soon as the probe reports the terminal Failed state.
wait_succeeded() { # description attempts probe probe_failed
	local description="$1" attempts="$2" probe="$3" probe_failed="$4" attempt=0
	while ! "$probe" >/dev/null 2>&1; do
		if "$probe_failed" >/dev/null 2>&1; then
			failure_dump "template-failed"
			fail "$description (template entered Failed)"
		fi
		attempt=$((attempt + 1))
		if [[ "$attempt" -ge "$attempts" ]]; then
			failure_dump "template-timeout"
			fail "$description (after $attempts attempts)"
		fi
		sleep 2
	done
	pass "$description"
}

template_up() {
	kubectl apply -f "$REPO_ROOT/config/samples/sandboxtemplate-firecracker.yaml" >/dev/null
	wait_succeeded "template phase=Succeeded" 300 template_succeeded template_failed
	local manifest_ref
	manifest_ref="$(kubectl_get "sandboxtemplate/$SBX_TEMPLATE" '{.status.manifestRef}')"
	[[ -n "$manifest_ref" ]] || fail "template manifestRef is empty"
	log "template manifestRef: $manifest_ref"
	assert_publish_layout
	pass "SandboxTemplate Succeeded + artifacts published"
}

assert_publish_layout() {
	local image_sha index_key index_json manifest_ref manifest_key manifest_json build_dir_key digest_size
	image_sha="$(printf '%s' "$SBX_IMAGE" | sha256sum | awk '{print $1}')"
	index_key="publish/index/$image_sha.json"
	index_json="$(mc cat "chain/$MINIO_BUCKET/$index_key")" || fail "index object missing: $index_key"
	[[ "$(printf '%s' "$index_json" | jq -r .image)" == "$SBX_IMAGE" ]] || fail "index.image does not match $SBX_IMAGE"
	manifest_ref="$(printf '%s' "$index_json" | jq -r .manifestRef)"
	[[ "$manifest_ref" == s3://* ]] || fail "manifestRef is not an s3 URL: $manifest_ref"
	manifest_key="${manifest_ref#s3://$MINIO_BUCKET/}"
	manifest_json="$(mc cat "chain/$MINIO_BUCKET/$manifest_key")" || fail "manifest object missing: $manifest_key"
	# index.artifactDigest is the sha256 of the manifest document. Hash the
	# raw object bytes (command substitution would strip the trailing
	# newline the digest was computed over).
	digest_size="$(printf '%s' "$index_json" | jq -r .artifactDigest)"
	[[ "$(mc cat "chain/$MINIO_BUCKET/$manifest_key" | sha256sum | awk '{print $1}')" == "$digest_size" ]] \
		|| fail "index.artifactDigest does not match sha256(manifest)"
	build_dir_key="$(dirname "$manifest_key")"
	for object in rootfs.ext4 vmstate.snap memory.snap SHA256SUMS manifest.json; do
		mc stat "chain/$MINIO_BUCKET/$build_dir_key/$object" >/dev/null 2>&1 \
			|| fail "artifact missing: $object"
	done
	# The manifest declares machine + guestNetwork (baked 172.30.0.3) and a
	# per-file sha256/sizeBytes list that must agree with the stored objects.
	[[ "$(printf '%s' "$manifest_json" | jq -r .guestNetwork.ip)" == "172.30.0.3" ]] \
		|| fail "manifest.guestNetwork.ip != 172.30.0.3"
	[[ "$(printf '%s' "$manifest_json" | jq -r .machine.vcpu)" == "1" ]] \
		|| fail "manifest.machine.vcpu != 1"
	local object
	while IFS= read -r object; do
		[[ -n "$object" ]] || continue
		local want_size stored_size
		want_size="$(printf '%s' "$manifest_json" | jq -r --arg n "$object" '.files[$n].sizeBytes')"
		[[ "$want_size" != "null" && -n "$want_size" ]] || fail "manifest.files has no entry for $object"
		stored_size="$(mc stat --json "chain/$MINIO_BUCKET/$build_dir_key/$object" 2>/dev/null | jq -r .size)"
		[[ "$stored_size" == "$want_size" ]] || fail "stored $object size $stored_size != manifest $want_size"
	done < <(printf '%s' "$manifest_json" | jq -r '.files | keys[]')
	pass "publish layout complete (index + manifest + artifacts, sizes match)"
}

# --- task 8: SandboxPool -----------------------------------------------------------------------
pool_up() {
	kubectl apply -f "$REPO_ROOT/config/samples/pool-firecracker.yaml" >/dev/null
	wait_for "fastlet pod running" 150 fastlet_pod_ready
	wait_for "pool warmImages Ready" 300 warm_images_ready
	pass "fastlet Running + warmImages Ready (agent PinImage closed loop)"
}

# --- task 9: sandbox + delivery ------------------------------------------------------------------
# --- fastctl: end-to-end driver through the central proxy ----------------------
# verify drives the full public surface with the host fastctl CLI: create /
# status / execd exec / delete all go through the Fast-Path gRPC API and the
# sandbox-proxy -> fastlet-proxy chain (never kubectl exec inside the
# fastlet). Port-forwards expose the in-cluster services to the host.
FASTCTL="$WORK/bin/fastctl"
GEN_ENDPOINT="$WORK/bin/gen-endpoint"
FASTPATH_LOCAL="127.0.0.1:19090"
SANDBOX_PROXY_LOCAL="http://127.0.0.1:18080"
FASTCTL_FLAGS=(--namespace "$NS" --endpoint "$FASTPATH_LOCAL" --proxy-endpoint "$SANDBOX_PROXY_LOCAL")

fastctl() { "$FASTCTL" "${FASTCTL_FLAGS[@]}" "$@"; }

PF_PIDS=()
GEN_DAEMON_PID=""
GEN_SOCKET="$WORK/gen-endpoint.sock"
FASTLET_IPS=()
FASTLET_PORTS=()

port_forward_up() {
	local pid
	[[ -x "$FASTCTL" ]] || die "fastctl not built ($FASTCTL); run up first"
	# A stale forward from an interrupted run holds the local ports and makes
	# the new kubectl port-forward exit immediately ("address already in
	# use"); sweep only forwards for OUR ports before starting.
	pkill -f "kubectl -n $NS port-forward .* 19090:9090" 2>/dev/null || true
	pkill -f "kubectl -n $NS port-forward .* 18[0-9][0-9]:5780" 2>/dev/null || true
	sleep 0.3
	kubectl -n "$NS" port-forward deploy/fast-sandbox-controller 19090:9090 >/dev/null 2>&1 &
	pid=$!
	PF_PIDS+=("$pid")
	# Direct-to-fastlet-proxy probing: one local port per fastlet pod, with
	# the pod IP -> port mapping for the DIRECT_FASTLET_PROXY endpoints.
	local port=18081 line name ip
	while read -r name ip; do
		[[ -n "$name" && -n "$ip" ]] || continue
		kubectl -n "$NS" port-forward "pod/$name" "$port:5780" >/dev/null 2>&1 &
		pid=$!
		PF_PIDS+=("$pid")
		FASTLET_IPS+=("$ip")
		FASTLET_PORTS+=("$port")
		port=$((port + 1))
	done < <(kubectl -n "$NS" get pods -l app=sandbox-fastlet \
		-o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.podIP}{"\n"}{end}' 2>/dev/null)
	wait_for "fastpath reachable via port-forward" 30 fastpath_reachable
}

# fastlet_local_port maps a fastlet pod IP (the DIRECT endpoint authority)
# to the local port-forward.
fastlet_local_port() { # pod-ip
	local ip="$1" i
	for i in "${!FASTLET_IPS[@]}"; do
		if [[ "${FASTLET_IPS[$i]}" == "$ip" ]]; then
			printf '%s' "${FASTLET_PORTS[$i]}"
			return 0
		fi
	done
	return 1
}

# resolve_daemon_up starts the resident route resolver: the FastPath gRPC
# connection is established once (no process spawn / HTTP2 dial per probe),
# so the measured data-plane latency reflects the chain, not the plumbing.
# Leftover daemons from interrupted runs are swept first (they hold the
# socket path); a stale binary is rebuilt on the spot.
resolve_daemon_up() {
	# A leftover daemon from an interrupted run keeps listening on the
	# socket path and interferes with readiness; sweep all of ours.
	pkill -f "gen-endpoint --daemon" 2>/dev/null || true
	sleep 0.3
	# grep without -q: -q closes the pipe on the first match and the writer
	# dies with SIGPIPE, which pipefail turns into a false "stale" verdict.
	if [[ ! -x "$GEN_ENDPOINT" ]] || ! "$GEN_ENDPOINT" 2>&1 | grep -- "--daemon" >/dev/null; then
		log "gen-endpoint binary missing or stale; rebuilding"
		write_gen_endpoint_source
		(cd "$REPO_ROOT" && GOTOOLCHAIN=local go build -o "$WORK/bin/gen-endpoint" .integration-env-gen/gen-endpoint.go)
	fi
	rm -f "$GEN_SOCKET"
	"$GEN_ENDPOINT" --daemon "$GEN_SOCKET" "127.0.0.1:19090" >/dev/null 2>&1 &
	GEN_DAEMON_PID=$!
	# readiness = the resolver actually answers, not just the socket node.
	wait_until "gen-endpoint daemon resolve" 10000 \
		sh -c "curl -fsS --unix-socket '$GEN_SOCKET' http://localhost/readyz >/dev/null 2>&1"
}

resolve_daemon_down() {
	if [[ -n "$GEN_DAEMON_PID" ]]; then
		kill "$GEN_DAEMON_PID" >/dev/null 2>&1 || true
		GEN_DAEMON_PID=""
	fi
	rm -f "$GEN_SOCKET"
}

# gen_endpoint_for resolves one port route through the resident daemon and
# prints "<proxy endpoint path>\t<route credential>".
gen_endpoint_for() { # sandbox-name port
	curl -fsS -m 15 --unix-socket "$GEN_SOCKET" \
		"http://localhost/resolve?ns=$NS&name=$1&port=$2" 2>/dev/null
}

port_forward_down() {
	local pid
	for pid in "${PF_PIDS[@]}"; do
		kill "$pid" >/dev/null 2>&1 || true
	done
	PF_PIDS=()
}

fastpath_reachable() {
	fastctl list >/dev/null 2>&1
}

sandbox_exists() { # sandbox-name
	fastctl get "$1" -o json >/dev/null 2>&1
}

sandbox_ready() { # sandbox-name
	# fastctl get -o json encodes the full GetSandboxResponse: the state
	# lives at .sandbox.runtime.state / .sandbox.data_plane.state (standard
	# encoding/json: underscore keys, numeric enums; READY == 4). The defs
	# also tolerate the bare SandboxInfo / protojson forms.
	fastctl get "$1" -o json 2>/dev/null | jq -e '
		def rt: (.runtime.state // .sandbox.runtime.state);
		def dp: ((.dataPlane.state // .data_plane.state) // (.sandbox.dataPlane.state // .sandbox.data_plane.state));
		(rt == 4 or rt == "RUNTIME_STATE_READY")
		and (dp == 4 or dp == "DATA_PLANE_STATE_READY")' >/dev/null 2>&1
}

fastctl_run_sandbox() { # sandbox-name
	local name="$1" attempt=0
	# Cold-start guarantee: a leftover sandbox from an interrupted run is
	# already warm and would invalidate the delivery-baseline measurement
	# (a 9ms run RPC with a 33ms restore is the tell-tale). Delete first;
	# leftovers of interrupted runs can tear down slowly, so after 90s the
	# cleanup finalizer is force-removed as a last resort.
	if sandbox_exists "$name"; then
		fastctl delete "$name" >/dev/null 2>&1 || true
		while ! sandbox_gone "$name"; do
			attempt=$((attempt + 1))
			if [[ "$attempt" -eq 90 ]]; then
				log "leftover $name still terminating after 90s; force-removing the cleanup finalizer"
				kubectl -n "$NS" patch sandbox "$name" --type=merge \
					-p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
			fi
			if [[ "$attempt" -ge 120 ]]; then
				fail "leftover $name could not be removed"
			fi
			sleep 1
		done
	fi
	# Creation can transiently fail while the pool scales out ("no eligible
	# Fastlet": the first fastlet is full and the second has not heartbeated
	# yet). Retry; a succeeded create that errored on the wire surfaces as
	# AlreadyExists on the next attempt, which is also success.
	for attempt in $(seq 1 30); do
		if fastctl run "$name" --image "$SBX_IMAGE" --pool "$SBX_POOL" >/dev/null 2>&1 \
			|| sandbox_exists "$name"; then
			return 0
		fi
		sleep 2
	done
	die "fastctl run $name failed"
}

sandbox_gone() { # sandbox-name
	! fastctl get "$1" -o json >/dev/null 2>&1
}

# probe_execd checks execd /ping END-TO-END through the central proxy: the
# resident resolver issues the sandbox PORT route (ResolveEndpoint 44772),
# then curl hits the sandbox-proxy with the credential — sandbox-proxy ->
# fastlet-proxy -> guest execd. The successful attempt records the resolve
# vs curl split in PROBE_RESOLVE_MS / PROBE_CURL_MS.
PROBE_RESOLVE_MS=""
PROBE_CURL_MS=""
PROBE_CONNECT_MS=""
PROBE_TTFB_MS=""
PROBE_LOG=()
probe_execd() { # sandbox-name
	local name="$1" out path cred uri host lport t0 timing t_abs
	t_abs=$(( $(now_ms) / 1000000 ))
	out="$(gen_endpoint_for "$name" 44772 2>/dev/null)" || {
		if [[ "${DEBUG_PROBE:-0}" == 1 ]]; then
			local errbody
			errbody="$(curl -s --unix-socket "$GEN_SOCKET" \
				"http://localhost/resolve?ns=$NS&name=$name&port=44772" 2>/dev/null)"
			echo "  [DEBUG_PROBE] $name: resolve FAILED body=${errbody:0:120}" >&2
		fi
		PROBE_LOG+=("t=${t_abs}ms resolve=FAILED")
		return 1
	}
	path="$(printf '%s' "$out" | cut -f1)"
	cred="$(printf '%s' "$out" | cut -f2)"
	[[ -n "$path" && -n "$cred" ]] || return 1
	PROBE_RESOLVE_MS="$(printf '%s' "$out" | cut -f3)"
	# strip scheme://authority -> /v1/sandboxes/{uid}/ports/{port}
	uri="$(printf '%s' "$path" | sed 's|^[a-z]*://[^/]*||')"
	# the DIRECT endpoint authority is the assigned fastlet pod IP; dial the
	# matching local port-forward instead.
	host="$(printf '%s' "$path" | sed 's|^[a-z]*://\([^/:]*\).*|\1|')"
	lport="$(fastlet_local_port "$host")" || {
		PROBE_LOG+=("t=${t_abs}ms host=${host} NO-LOCAL-PORT")
		return 1
	}
	t0="$(now_ms)"
	timing="$(curl -fsS -m 5 -o /dev/null \
		-w '%{time_connect} %{time_starttransfer}' \
		-H "X-Fast-Sandbox-Route-Credential: $cred" \
		"http://127.0.0.1:$lport$uri/ping" 2>/dev/null)" || {
		# DEBUG_PROBE=1: surface the failure window (code + body) instead of
		# silently retrying.
		local code
		if [[ "${DEBUG_PROBE:-0}" == 1 ]]; then
			code="$(curl -s -o /dev/null -w '%{http_code}' \
				-H "X-Fast-Sandbox-Route-Credential: $cred" \
				"http://127.0.0.1:$lport$uri/ping" 2>/dev/null)"
		else
			code="?"
		fi
		PROBE_LOG+=("t=${t_abs}ms resolve=${PROBE_RESOLVE_MS} host=${host} curl=HTTP${code}")
		return 1
	}
	PROBE_CURL_MS=$(( ($(now_ms) - t0) / 1000000 ))
	# time_connect / time_starttransfer are seconds since the request start.
	PROBE_CONNECT_MS="$(printf '%s' "$timing" | awk '{printf "%d", $1 * 1000}')"
	PROBE_TTFB_MS="$(printf '%s' "$timing" | awk '{printf "%d", $2 * 1000}')"
	PROBE_LOG+=("t=${t_abs}ms resolve=${PROBE_RESOLVE_MS} host=${host} curl=200 ttfb=${PROBE_TTFB_MS}")
}

# show_ping_latency splits the proxy-chain /ping cost: one route resolution
# through the resident resolver, then a COLD curl (fresh TCP through
# port-forward + sandbox-proxy + fastlet-proxy) and a WARM curl reusing the
# same route and connection setup path (still a new TCP per hop, but no
# resolve/sign).
show_ping_latency() { # sandbox-name
	local name="$1" out path cred uri host lport t0 cold_ms warm_ms
	out="$(gen_endpoint_for "$name" 44772 2>/dev/null)" || return 0
	path="$(printf '%s' "$out" | cut -f1)"
	cred="$(printf '%s' "$out" | cut -f2)"
	[[ -n "$path" && -n "$cred" ]] || return 0
	uri="$(printf '%s' "$path" | sed 's|^[a-z]*://[^/]*||')"
	host="$(printf '%s' "$path" | sed 's|^[a-z]*://\([^/:]*\).*|\1|')"
	lport="$(fastlet_local_port "$host")" || return 0
	t0="$(now_ms)"
	curl -fsS -m 5 -o /dev/null \
		-H "X-Fast-Sandbox-Route-Credential: $cred" \
		"http://127.0.0.1:$lport$uri/ping" 2>/dev/null || return 0
	cold_ms=$(( ($(now_ms) - t0) / 1000000 ))
	t0="$(now_ms)"
	curl -fsS -m 5 -o /dev/null \
		-H "X-Fast-Sandbox-Route-Credential: $cred" \
		"http://127.0.0.1:$lport$uri/ping" 2>/dev/null || return 0
	warm_ms=$(( ($(now_ms) - t0) / 1000000 ))
	highlight "  key node: proxy-chain /ping latency of '$name' (same route reused)"
	printf '    cold /ping = %sms   warm /ping = %sms\n' "$cold_ms" "$warm_ms" | tee -a "$WORK/run.log"
}

# klog_field extracts one key="value" (or key=value for ints) pair from a
# klog text line.
klog_field() { # line key
	local raw
	raw="$(printf '%s' "$1" | grep -o "$2=\"[^\"]*\"\|$2=[0-9a-zA-Z._-]*" | head -1)"
	[[ -n "$raw" ]] || return 0
	printf '%s' "${raw#*=}" | tr -d '"'
}

# show_restore_timings highlights the driver's per-sandbox creation breakdown
# (total/acquire/rootfs/infra/launch/configure/boot) from the fastlet logs.
show_restore_timings() { # sandbox-name
	local name="$1" fastlet line
	fastlet="$(kubectl_get "sandbox/$name" '{.status.placement.fastletName}')"
	[[ -n "$fastlet" ]] || return 0
	line="$(kubectl -n "$NS" logs --request-timeout=10s --tail=300 "$fastlet" 2>/dev/null | grep 'firecracker sandbox created' | tail -1)"
	[[ -n "$line" ]] || return 0
	highlight "  key node: golden restore of '$name'"
	printf '    total=%s  acquire=%s  rootfs=%s  infra=%s  launch=%s  configure=%s  boot=%s  vmStatePolls=%s\n' \
		"$(klog_field "$line" total)" "$(klog_field "$line" acquire)" \
		"$(klog_field "$line" rootfs)" "$(klog_field "$line" infra)" \
		"$(klog_field "$line" launch)" "$(klog_field "$line" configure)" \
		"$(klog_field "$line" boot)" "$(klog_field "$line" vmStatePolls)"
}

# report_create_tail derives the end-to-end create tail from three
# independent clocks: fastctl run RPC (client), fastpath create (controller),
# golden restore (fastlet driver). tail = everything after the VM resumed
# until the client sees READY. restore_total and fastpath_total are parsed
# from their own components' logs (no shared instrumentation).
report_create_tail() { # name t0-ns t-run-done-ns
	local name="$1" t0="$2" t_done="$3"
	local run_rpc fp_total dr_total fp_line dr_line fastlet
	run_rpc=$(( (t_done - t0) / 1000000 ))
	fp_line="$(kubectl -n "$NS" logs --request-timeout=10s --tail=500 deploy/fast-sandbox-controller 2>/dev/null | grep 'fastpath sandbox created' | grep "requestId=\"$name\"" | tail -1)"
	[[ -n "$fp_line" ]] || return 0
	fp_total="$(klog_field "$fp_line" total | tr -d 'ms')"
	fp_total="${fp_total%%.*}"
	[[ -n "$fp_total" ]] || return 0
	fastlet="$(kubectl_get "sandbox/$name" '{.status.placement.fastletName}')"
	[[ -n "$fastlet" ]] || return 0
	dr_line="$(kubectl -n "$NS" logs --request-timeout=10s --tail=300 "$fastlet" 2>/dev/null | grep 'firecracker sandbox created' | tail -1)"
	dr_total="$(klog_field "$dr_line" total | tr -d 'ms')"
	dr_total="${dr_total%%.*}"
	[[ -n "$dr_total" ]] || dr_total=0
	highlight "  key node: end-to-end create tail of '$name' (post-restore)"
	printf '    create tail = %sms   fastlet-side = %sms   client gap = %sms   (run RPC %sms + restore %sms)\n' \
		"$(( run_rpc - dr_total ))" "$(( fp_total - dr_total ))" "$(( run_rpc - fp_total ))" \
		"$run_rpc" "$dr_total" | tee -a "$WORK/run.log"
}

# show_e2e_latency reports the true end-to-end timeline of one sandbox:
# fastctl run (fastpath create + fastlet restore, completion=READY blocks
# until the runtime reports ready) -> first successful execd /ping through
# the central proxy. Availability is judged by the probe, NOT by the
# eventually-consistent CR status. Timestamps in epoch-ms, deltas in ms.
show_e2e_latency() { # sandbox-name t0-ns t-run-done-ns t-ping-ns t-probe-start-ns
	local name="$1" t0="$2" t_run_done="$3" t_ping="$4" t_probe_start="$5"
	local e0 er ep run_rpc queue first200 total
	e0=$(( t0 / 1000000 ))
	er=$(( t_run_done / 1000000 ))
	ep=$(( t_ping / 1000000 ))
	run_rpc=$(( (t_run_done - t0) / 1000000 ))
	# queue = time between the create returning and THIS sandbox's probe
	# beginning (sequential probing: earlier sandboxes' probes/reporting
	# land here, so the first-200 wait below is the honest per-sandbox cost).
	queue=$(( (t_probe_start - t_run_done) / 1000000 ))
	first200=$(( (t_ping - t_probe_start) / 1000000 ))
	total=$(( (t_ping - t0) / 1000000 ))
	highlight "  key node: end-to-end latency of '$name' (run → execd /ping)"
	printf '    t(run)=%sms  t(run-done)=%sms  t(probe)=%sms  t(ping)=%sms\n' "$e0" "$er" "$(( t_probe_start / 1000000 ))" "$ep"
	printf '    run RPC = %sms   queue-to-probe = %sms   first-200 = %sms   total = %sms\n' \
		"$run_rpc" "$queue" "$first200" "$total" | tee -a "$WORK/run.log"
	if [[ -n "$PROBE_RESOLVE_MS" ]]; then
		printf '    first probe: resolve = %sms   connect = %sms   ttfb = %sms   curl-total = %sms\n' \
			"$PROBE_RESOLVE_MS" "$PROBE_CONNECT_MS" "$PROBE_TTFB_MS" "$PROBE_CURL_MS" | tee -a "$WORK/run.log"
	fi
	if [[ "${DEBUG_PROBE:-0}" == 1 && ${#PROBE_LOG[@]} -gt 1 ]]; then
		printf '    probe attempts: %s\n' "${PROBE_LOG[*]}" >&2
	fi
}

verify() {
	local second="${SBX_SANDBOX}-2"

	port_forward_up
	resolve_daemon_up
	trap 'port_forward_down; resolve_daemon_down' EXIT
	run_stage "verify 1: sandbox create + execd /ping (fastctl)" verify_sandbox "$SBX_SANDBOX"
	run_stage "verify 2: clone sandbox (shared snapshot, per-clone netns)" verify_sandbox "$second"
	run_stage "verify 3: max concurrency (2 fastlets, 10 slots)" verify_concurrent
	run_stage "verify 4: delete all ($((CONCURRENCY + 2))) + cleanup" verify_delete_all
	trap - EXIT
	resolve_daemon_down
	port_forward_down

	stage_summary
	highlight "== verify complete: 2 + $CONCURRENCY sandboxes delivered via fastctl (central proxy); teardown clean =="
}

# --- max-concurrency soak (part of verify) ------------------------------------
# CONCURRENCY (default 5) is the per-fastlet slot capacity
# (maxSandboxesPerPod). The batch runs while the two probe sandboxes are
# still alive: 7 sandboxes exceed one fastlet's 5 slots, so the pool keeps
# two fastlet pods (poolMin=2, 10 slots) and the batch lands on both —
# proving concurrent restore across multiple fastlets on the shared golden
# snapshot. (Dynamic scale-out cannot trigger via the fastpath create: a
# full pool is rejected with ResourceExhausted BEFORE the Sandbox CR exists,
# so the controller never sees the pending demand.)
# Names use the sbx-stress- prefix so they never collide with the verify
# sandboxes.
CONCURRENCY="${CONCURRENCY:-5}"

stress_name() { printf 'sbx-stress-%d' "$1"; }

fastlet_pod_count() { # expected-count
	local got
	got="$(kubectl -n "$NS" get pods -l app=sandbox-fastlet -o jsonpath='{.items[*].metadata.name}' 2>/dev/null | wc -w | tr -d ' ')"
	[[ "$got" -eq "$1" ]]
}

verify_concurrent() {
	local i name t_run_done t_probe_start t_ping
	local -a t0s=() t_dones=() t_probes=() t_pings=()
	if [[ "$CONCURRENCY" -gt 5 ]]; then
		die "CONCURRENCY=$CONCURRENCY exceeds the pool's maxSandboxesPerPod=5"
	fi
	for i in $(seq 1 "$CONCURRENCY"); do
		name="$(stress_name "$i")"
		t0s+=("$(now_ms)")
		fastctl_run_sandbox "$name"
		t_dones+=("$(now_ms)")
	done
	log "created $CONCURRENCY sandboxes on top of 2 running sandboxes"
	wait_for "two fastlet pods ready (poolMin=2 topology)" 180 fastlet_pod_count 2
	pass "2 fastlet pods serving $((CONCURRENCY + 2)) sandboxes (2 x 5 slots)"
	# Sequential probing with probe-start-relative timing: every sandbox
	# records its own first-200 wait from the moment ITS probe began, so the
	# reported value is never polluted by the earlier sandboxes' probes or
	# reporting. (Parallel subshell probing was dropped: it kept hanging on
	# the shared daemon/port-forward background jobs.)
	for i in $(seq 1 "$CONCURRENCY"); do
		name="$(stress_name "$i")"
		t_probe_start="$(now_ms)"
		wait_until "execd /ping $name" 120000 probe_execd "$name"
		t_ping="$(now_ms)"
		show_e2e_latency "$name" "${t0s[$((i - 1))]}" "${t_dones[$((i - 1))]}" "$t_ping" "$t_probe_start"
		show_restore_timings "$name"
		report_create_tail "$name" "${t0s[$((i - 1))]}" "${t_dones[$((i - 1))]}"
		show_ping_latency "$name"
	done
	pass "$CONCURRENCY sandboxes Ready (concurrent VMs from the shared snapshot)"
	pass "execd /ping reachable on all $CONCURRENCY sandboxes (via central proxy)"
}

verify_sandbox() { # sandbox-name
	local name="$1" t0 t_run_done t_probe_start t_ping
	t0="$(now_ms)"
	fastctl_run_sandbox "$name"
	t_run_done="$(now_ms)"
	# Availability is judged END-TO-END: the first successful execd /ping
	# through the central proxy, not the eventually-consistent CR status
	# (fastpath create already blocked on completion=READY, so the route is
	# resolvable as soon as run returns).
	t_probe_start="$(now_ms)"
	wait_until "execd /ping" 120000 probe_execd "$name"
	t_ping="$(now_ms)"
	show_e2e_latency "$name" "$t0" "$t_run_done" "$t_ping" "$t_probe_start"
	show_restore_timings "$name"
	report_create_tail "$name" "$t0" "$t_run_done"
	show_ping_latency "$name"
	if sandbox_ready "$name"; then
		log "    CR status Ready confirmed (eventual consistency)"
	fi
	pass "execd /ping reachable on $name (via central proxy)"
}

verify_delete_all() {
	local i name
	for name in "$SBX_SANDBOX" "${SBX_SANDBOX}-2"; do
		fastctl delete "$name" >/dev/null 2>&1 || true
	done
	for i in $(seq 1 "$CONCURRENCY"); do
		name="$(stress_name "$i")"
		fastctl delete "$name" >/dev/null 2>&1 || true
	done
	wait_for "agent leases drained" 120 agent_leases_drained
	wait_for "jail dirs cleaned" 120 jails_cleaned
	pass "delete cleaned leases + jails ($((CONCURRENCY + 2)) sandboxes)"
}

# --- status --------------------------------------------------------------------------------------
status() {
	log "status: components"
	kubectl -n "$NS" get pods -o wide 2>/dev/null || true
	echo
	log "status: SandboxTemplate"
	kubectl -n "$NS" get sandboxtemplate -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,MANIFEST:.status.manifestRef' 2>/dev/null || true
	echo
	log "status: SandboxPool"
	kubectl -n "$NS" get sandboxpool -o custom-columns='NAME:.metadata.name,RUNTIME:.spec.runtime,WARM_IMAGES:.status.warmImages' 2>/dev/null || true
	echo
	log "status: Sandboxes"
	kubectl -n "$NS" get sandbox -o wide 2>/dev/null || true
	echo
	log "status: MinIO"
	docker ps --filter "name=$MINIO_CONTAINER" --format '{{.Names}} {{.Status}}' 2>/dev/null || true
	if kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" >/dev/null; then
		log "kind cluster: $KIND_CLUSTER (up)"
	else
		log "kind cluster: down"
	fi
}

# --- down ---------------------------------------------------------------------------------------
down() {
	log "down: teardown"
	if kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" >/dev/null; then
		kind delete cluster --name "$KIND_CLUSTER" > "$LOGS_DIR/kind-delete.log" 2>&1 || true
	fi
	[[ -z "$(kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" || true)" ]] \
		|| fail "kind cluster $KIND_CLUSTER still exists after delete"
	docker rm -f "$MINIO_CONTAINER" >/dev/null 2>&1 || true
	[[ -z "$(docker ps -a --filter "name=$MINIO_CONTAINER" --format '{{.Names}}' || true)" ]] \
		|| fail "MinIO container still present"
	rm -f "$WORK/agent-registry.json" "$WORK/registry.json"
	rm -rf "$GEN_DIR"
	sysctl_restore
	stateroot_xfs_down
	if docker network ls --format '{{.Name}}' | grep -qx 'kind'; then
		log "note: docker network 'kind' remains (kind-wide, reused on next up)"
	fi
	pass "host cleanup complete"
}

# --- main ------------------------------------------------------------------------------------------
# env_summary prints the reachable facts of the environment for hand-off.
env_summary() {
	highlight "== environment summary =="
	printf '  %-20s %s\n' "MinIO endpoint" "$MINIO_ENDPOINT"
	printf '  %-20s %s (execd=%s)\n' "image" "$SBX_IMAGE" "$EXECD"
	printf '  %-20s %s\n' "manifestRef" "$(kubectl_get "sandboxtemplate/$SBX_TEMPLATE" '{.status.manifestRef}' 2>/dev/null || true)"
	printf '  %-20s %s\n' "template phase" "$(kubectl_get "sandboxtemplate/$SBX_TEMPLATE" '{.status.phase}' 2>/dev/null || true)"
	printf '  %-20s %s\n' "fastlet pod" "$(kubectl -n "$NS" get pods -l app=sandbox-fastlet -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
	printf '  %-20s %s\n' "warmImages" "$(kubectl -n "$NS" get sandboxpool "$SBX_POOL" -o jsonpath='{.status.warmImages[*].image}' 2>/dev/null || true)"
	printf '  %-20s %s\n' "StateRoot fs" "$(findmnt -no FSTYPE "$XFS_MOUNT_POINT" 2>/dev/null || echo 'ext4 (full copy per sandbox)')"
	printf '  %-20s %s\n' "logs" "$LOGS_DIR"
}

usage() {
	cat <<'EOF'
usage: integration-env.sh [--cleanup|--auto-clean] {up|down|status|verify}

  up       build the whole environment (tasks 1-9) and report status
  down     teardown: kind cluster + MinIO container + credentials + sysctl
  status   component / template / pool / sandbox health
  verify   create 2 sandboxes, probe execd /ping, then a max-concurrency
           batch (CONCURRENCY=5 at pool capacity), delete all, assert cleanup

  --cleanup     down after an interrupted run (same recovery as down)
  --auto-clean  on up failure, run down automatically before dumping logs
EOF
	exit 1
}

for arg in "$@"; do
	case "$arg" in
		--cleanup) ACTION="down" ;;
		--auto-clean) AUTO_CLEAN=1 ;;
		up|down|status|verify) ACTION="$arg" ;;
		*) usage ;;
	esac
done
[[ -n "$ACTION" ]] || usage

mkdir -p "$WORK" "$LOGS_DIR"

case "$ACTION" in
	up)
		exec > >(tee -a "$WORK/run.log") 2>&1
		log "=== integration-env up ($(date -u +%FT%TZ)) ==="
		{
			echo "environment snapshot ($(date -u +%FT%TZ))"
			command -v kind >/dev/null && kind --version
			kubectl version --client 2>/dev/null | head -1
			go version
			docker --version
			echo "minio=$MINIO_IMAGE minioPort=$MINIO_PORT bucket=$MINIO_BUCKET"
			echo "fcVersion=$FC_VERSION sbxImage=$SBX_IMAGE execd=$EXECD"
			echo "images: controller=$IMG_CONTROLLER agent=$IMG_AGENT builder=$IMG_BUILDER"
		} > "$LOGS_DIR/environment.txt" 2>&1 || true
		if [[ -n "$(kind get clusters 2>/dev/null | grep -x "$KIND_CLUSTER" || true)" ]] \
			|| docker ps -a --format '{{.Names}}' | grep -qx "$MINIO_CONTAINER"; then
			if [[ "$SKIP_LEFTOVER_CLEAN" == 1 ]]; then
				log "leftover resources detected; aborting (SKIP_LEFTOVER_CLEAN=1). Run 'integration-env.sh down' first"
				exit 1
			fi
			log "leftover resources detected; cleaning and rebuilding"
			down
		fi
		trap 'on_error up' ERR
		run_stage "task 1: preflight + tooling" preflight
		run_stage "task 1: sysctl (fs.inotify)" sysctl_set
		run_stage "task 1: build images (7)" build_images
		run_stage "task 1: XFS StateRoot (reflink)" stateroot_xfs_up
		run_stage "task 2: kind cluster (KVM passthrough)" kind_up
		run_stage "task 3: MinIO + bucket" minio_up
		run_stage "task 3: MinIO endpoint (kind network)" resolve_minio_endpoint
		run_stage "task 4: CRDs + controller" controller_up
		run_stage "task 3: credentials (publish/pull)" credentials_up
		run_stage "task 5: firecracker node assets" installer_up
		run_stage "task 6: runtime-agent DaemonSet" agent_up
		run_stage "task 7: SandboxTemplate build" template_up
		run_stage "task 8: SandboxPool + warmImages" pool_up
		trap - ERR
		stage_summary
		env_summary
		highlight "== up complete: run 'integration-env.sh verify' (or 'status') =="
		;;
	verify)
		trap 'on_error verify' ERR
		verify
		trap - ERR
		;;
	status)
		status
		;;
	down)
		down
		;;
esac

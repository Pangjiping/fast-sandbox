#!/usr/bin/env bash
# dbg-proxy-concurrent.sh — reproduce the batch first-probe failure window.
#
# Mirrors the verify batch exactly: PRESEED resident sandboxes settle first
# (verify has the two probe sandboxes), then N concurrent creates launch, and
# probing starts right after (default 0.3s) at 50ms poll, reporting every
# failure code plus the time-to-first-200 per sandbox.
set -u
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FASTCTL="$REPO/.integration-env/bin/fastctl"
GEN="$REPO/.integration-env/bin/gen-endpoint"
NS=fast-sandbox-system
EP=127.0.0.1:19090
PROXY=http://127.0.0.1:18080
PRESEED="${PRESEED:-1}"   # create 1 resident sandbox first (verify batch has 2)
N="${1:-5}"           # concurrent sandboxes
SETTLE="${2:-0.3}"    # seconds between launch and probing
POLL="${3:-0.05}"     # probe poll interval in seconds

cleanup() {
  kill "$PF1" "$PF2" 2>/dev/null
  for n in $(seq 1 "$N"); do
    timeout 10 "$FASTCTL" -n "$NS" --endpoint "$EP" --proxy-endpoint "$PROXY" delete "dbg-$n" >/dev/null 2>&1
  done
  for n in $(seq 1 "$PRESEED"); do
    timeout 10 "$FASTCTL" -n "$NS" --endpoint "$EP" --proxy-endpoint "$PROXY" delete "seed-$n" >/dev/null 2>&1
  done
}
trap cleanup EXIT

kubectl -n "$NS" port-forward deploy/fast-sandbox-controller 19090:9090 >/dev/null 2>&1 &
PF1=$!
kubectl -n "$NS" port-forward svc/fast-sandbox-proxy 18080:8080 >/dev/null 2>&1 &
PF2=$!
for i in $(seq 1 30); do
  curl -fsS http://127.0.0.1:18080/readyz >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS http://127.0.0.1:18080/readyz >/dev/null || { echo "forwards not ready"; exit 1; }
echo "== forwards up =="

echo "== creating $PRESEED resident sandbox(s), settling 3s =="
for n in $(seq 1 "$PRESEED"); do
  timeout 30 "$FASTCTL" -n "$NS" --endpoint "$EP" --proxy-endpoint "$PROXY" run "seed-$n" --image alpine:3.19 --pool firecracker-pool >/dev/null 2>&1
done
sleep 3

echo "== launching $N concurrent creates (async, bounded 30s) =="
for n in $(seq 1 "$N"); do
  ( timeout 30 "$FASTCTL" -n "$NS" --endpoint "$EP" --proxy-endpoint "$PROXY" run "dbg-$n" --image alpine:3.19 --pool firecracker-pool \
      >/dev/null 2>&1; echo "run dbg-$n exit=$?" ) &
done

echo "== settling ${SETTLE}s then probing each sandbox (50ms poll, 15s window) =="
sleep "$SETTLE"

for n in $(seq 1 "$N"); do
  echo "=== dbg-$n ==="
  launched_ms=$(( $(date +%s%N) / 1000000 ))
  first_200=""
  failures=0
  successes=0
  while :; do
    now_ms=$(( $(date +%s%N) / 1000000 ))
    if (( now_ms - launched_ms > 15000 )); then
      echo "  TIMEOUT after 15s (last status: ${last:-none})"
      break
    fi
    OUT=$("$GEN" "$EP" "$NS" "dbg-$n" 44772 2>/dev/null)
    if [[ -z "$OUT" ]]; then
      failures=$((failures + 1))
      if (( failures <= 5 )); then
        echo "  +$((now_ms - launched_ms))ms resolve FAILED"
      fi
      sleep "$POLL"
      continue
    fi
    URI=$(printf '%s' "$OUT" | cut -f1 | sed 's|^[a-z]*://[^/]*||')
    CRED=$(printf '%s' "$OUT" | cut -f2)
    CODE=$(timeout 3 curl -s -o /dev/null -w '%{http_code}' \
      -H "X-Fast-Sandbox-Route-Credential: $CRED" "http://127.0.0.1:18080$URI/ping")
    last="$CODE"
    if [[ "$CODE" == "200" ]]; then
      if [[ -z "$first_200" ]]; then
        first_200=$((now_ms - launched_ms))
        echo "  FIRST 200 at +${first_200}ms"
      fi
      # two consecutive 200s = settled
      if (( successes++ >= 1 )); then
        break
      fi
    else
      successes=0
      failures=$((failures + 1))
      if (( failures <= 10 )); then
        echo "  +$((now_ms - launched_ms))ms HTTP $CODE"
      fi
    fi
    sleep "$POLL"
  done
done
echo "== done =="

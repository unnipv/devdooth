#!/usr/bin/env bash
# Local smoke test: coordinator + worker + a real Playwright client over CDP.
#
#   ./scripts/smoke.sh
#
# Requires: go, node, npm install (for playwright-core), and Chrome/Chromium.
set -euo pipefail

cd "$(dirname "$0")/.."

ADDR="127.0.0.1:18080"
ADMIN_TOKEN="smoke-admin"
WORKER_TOKEN="smoke-worker"
DATA_DIR="$(mktemp -d)"
COORD_PID=""
WORKER_PID=""

cleanup() {
  [[ -n "$WORKER_PID" ]] && kill "$WORKER_PID" 2>/dev/null || true
  [[ -n "$COORD_PID" ]] && kill "$COORD_PID" 2>/dev/null || true
  wait 2>/dev/null || true
  rm -rf "$DATA_DIR"
}
trap cleanup EXIT

echo "==> building"
go build -o bin/devdooth ./cmd/devdooth

echo "==> starting coordinator on $ADDR"
bin/devdooth coordinator --addr "$ADDR" --admin-token "$ADMIN_TOKEN" --worker-token "$WORKER_TOKEN" \
  >"$DATA_DIR/coordinator.log" 2>&1 &
COORD_PID=$!
for _ in $(seq 1 50); do
  curl -fsS "http://$ADDR/healthz" >/dev/null 2>&1 && break
  sleep 0.1
done

echo "==> starting worker"
bin/devdooth worker --coordinator "http://$ADDR" --token "$WORKER_TOKEN" \
  --name macbook --data-dir "$DATA_DIR/worker" --max-slots 1 \
  >"$DATA_DIR/worker.log" 2>&1 &
WORKER_PID=$!
sleep 2

echo
echo "==> nodes"
bin/devdooth nodes --url "http://$ADDR" --token "$ADMIN_TOKEN"

echo
echo "==> leasing a browser"
LEASE_JSON="$(bin/devdooth lease --url "http://$ADDR" --token "$ADMIN_TOKEN" --node macbook --ttl 120 2>/dev/null)"
echo "$LEASE_JSON"
ENDPOINT="$(printf '%s' "$LEASE_JSON" | python3 -c 'import sys,json; print(json.load(sys.stdin)["endpoint"])')"
LEASE_ID="$(printf '%s' "$LEASE_JSON" | python3 -c 'import sys,json; print(json.load(sys.stdin)["lease_id"])')"

echo
echo "==> driving the leased browser with Playwright"
node scripts/e2e-playwright.mjs "$ENDPOINT"

echo
echo "==> releasing"
bin/devdooth release --url "http://$ADDR" --token "$ADMIN_TOKEN" --lease "$LEASE_ID"

echo
echo "==> nodes after release"
bin/devdooth nodes --url "http://$ADDR" --token "$ADMIN_TOKEN"

if grep -a -i "remote-debugging" "$DATA_DIR"/worker.log >/dev/null 2>&1; then
  echo "note: worker log mentions remote debugging; check it is loopback-only"
fi

echo "smoke test passed"

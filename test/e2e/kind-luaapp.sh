#!/usr/bin/env bash
#
# End-to-end test that runs a user Lua application inside the cluster. The app is
# supplied from a ConfigMap mounted at /app and loaded via Tarantool 3's
# `app.file` (set through spec.config). The test then drives that user code over
# the admin console:
#   * the app's load marker is set (app.file actually ran),
#   * a user function computes correctly,
#   * user data-plane logic creates a space and stores/reads a row.
#
# Runs the operator out-of-cluster, same as the other e2e scripts.
#
# Usage:
#   test/e2e/kind-luaapp.sh        # create, test, tear down
#   KEEP=1 test/e2e/kind-luaapp.sh # leave the cluster up
#
# Requires: kind, kubectl, go, and a running Docker daemon.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-luaapp}"
OP_LOG="$(mktemp)"
OP_PID=""

cleanup() {
  set +e
  [ -n "$OP_PID" ] && kill "$OP_PID" 2>/dev/null
  if [ -n "${KEEP:-}" ]; then
    echo "==> KEEP set: leaving kind cluster '$CLUSTER' running (operator stopped)"
  else
    echo "==> tearing down"
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
  fi
  rm -f "$OP_LOG"
}
trap cleanup EXIT

fail() { echo "FAIL: $*"; kubectl get pods,sts 2>/dev/null; exit 1; }

# tt_eval <pod> <lua>: run a Lua expression over the admin console, print output.
tt_eval() {
  kubectl exec "$1" -- sh -c "echo '$2' | tt connect /var/run/tarantool/admin.socket 2>/dev/null" 2>/dev/null
}

for bin in kind kubectl go; do
  command -v "$bin" >/dev/null || { echo "missing required tool: $bin"; exit 1; }
done

echo "==> create kind cluster '$CLUSTER'"
kind create cluster --name "$CLUSTER" >/dev/null

echo "==> install CRDs"
kubectl apply -R -f config/crd/bases/ >/dev/null

echo "==> build and start operator (out-of-cluster)"
go build -o bin/manager .
./bin/manager --metrics-bind-address=0 --health-probe-bind-address=0 >"$OP_LOG" 2>&1 &
OP_PID=$!
for _ in $(seq 1 30); do
  # Ready once the db.tarantool.io controllers have started (robust to the
  # total controller count and to other controllers' log wording).
  grep -q 'Starting workers.*db.tarantool.io' "$OP_LOG" 2>/dev/null && break
  kill -0 "$OP_PID" 2>/dev/null || { echo "operator exited early:"; cat "$OP_LOG"; exit 1; }
  sleep 2
done

echo "==> apply cluster with a user Lua app (app.file from a mounted ConfigMap)"
kubectl apply -f test/e2e/testdata/luaapp.yaml >/dev/null
for _ in $(seq 1 30); do
  kubectl get statefulset app-rs >/dev/null 2>&1 && break
  sleep 2
done
echo "==> wait for the instance to be Ready"
kubectl rollout status statefulset/app-rs --timeout=240s || {
  kubectl describe pod app-rs-0 2>/dev/null | tail -20
  fail "instance did not become ready"
}

echo "==> assert the user app.file actually loaded"
out="$(tt_eval app-rs-0 'return rawget(_G, "e2e_app_loaded")')"
echo "$out" | grep -q 'true' || fail "user app did not load (e2e_app_loaded not true): $out"

echo "==> assert a user function runs"
out="$(tt_eval app-rs-0 'return e2e_sum(40, 2)')"
echo "$out" | grep -qE '(^|[^0-9])42([^0-9]|$)' || fail "user function e2e_sum(40,2) != 42: $out"

echo "==> assert user data-plane logic (create space, store + read a row)"
out="$(tt_eval app-rs-0 'return e2e_seed()')"
echo "$out" | grep -q 'widget' || fail "user seed logic did not return the stored value: $out"
out="$(tt_eval app-rs-0 'return box.space.items:count()')"
echo "$out" | grep -qE '(^|[^0-9])1([^0-9]|$)' || fail "expected 1 row in space items: $out"

echo "PASS: user Lua application e2e succeeded"

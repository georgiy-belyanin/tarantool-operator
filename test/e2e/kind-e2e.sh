#!/usr/bin/env bash
#
# Simple end-to-end smoke test for the Tarantool 3 (db.tarantool.io/v2alpha1)
# operator against a real kind cluster.
#
# It creates a kind cluster, installs the CRDs, runs the operator out-of-cluster
# (so it does not depend on building/pushing an image), applies a minimal
# single-instance cluster, and asserts that the operator drives it to a running,
# Ready Tarantool 3 instance — exercising the full render -> Secret -> mounted
# config -> TT_CONFIG/TT_INSTANCE_NAME -> boot -> box.info readiness pipeline.
#
# Usage:
#   test/e2e/kind-e2e.sh        # create, test, tear down
#   KEEP=1 test/e2e/kind-e2e.sh # leave the cluster up for inspection
#
# Requires: kind, kubectl, go, and a running Docker daemon.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e}"
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

fail() { echo "FAIL: $*"; kubectl get pods,sts,replicaset.db.tarantool.io 2>/dev/null; exit 1; }

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

echo "==> apply sample cluster"
kubectl apply -f test/e2e/testdata/smoke.yaml >/dev/null

echo "==> wait for the StatefulSet to be created"
for _ in $(seq 1 30); do
  kubectl get statefulset e2e-rs >/dev/null 2>&1 && break
  sleep 2
done
kubectl get statefulset e2e-rs >/dev/null 2>&1 || fail "operator did not create the StatefulSet"

echo "==> wait for rollout (pulls the tarantool image, boots, passes readiness)"
kubectl rollout status statefulset/e2e-rs --timeout=240s || fail "StatefulSet did not become ready"

echo "==> assert ReplicaSet status is Ready"
phase=""
for _ in $(seq 1 30); do
  phase="$(kubectl get replicaset.db.tarantool.io e2e-rs -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  [ "$phase" = "Ready" ] && break
  sleep 2
done
[ "$phase" = "Ready" ] || fail "ReplicaSet phase=$phase (want Ready)"

echo "==> assert box.info.status == running"
kubectl exec e2e-rs-0 -- sh -c \
  "echo 'return box.info.status' | tt connect /var/run/tarantool/admin.socket 2>/dev/null" \
  | grep -q running || fail "box.info.status is not 'running'"

echo "PASS: e2e smoke test succeeded"

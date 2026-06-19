#!/usr/bin/env bash
#
# Tests the shipped samples (config/samples). Two layers:
#
#   1. RENDER: every samples kustomization (tarantool3 + cartridge) must build.
#   2. RUNTIME: each sample that is runnable on the stock community
#      tarantool/tarantool:3 image is applied to a kind cluster sequentially and
#      must converge (StatefulSets fully ready), then is deleted:
#        - minimal     (1 instance)
#        - replicated  (3 instances, manual failover)
#        - roles       (1 instance + application role)
#        - advanced    (3 replica sets / 2 groups: role + Lua app + Secrets + PVCs)
#
#      Two samples are render-validated only, by design:
#        - the sharded trio needs a vshard-bundled image (community image lacks it);
#        - metrics needs a metrics-export-capable image AND the Prometheus
#          Operator CRDs (ServiceMonitor) that a plain kind cluster doesn't have.
#
# Usage:
#   test/e2e/kind-samples.sh        # create, test, tear down
#   KEEP=1 test/e2e/kind-samples.sh # leave the cluster up
#
# Requires: kind, kubectl, go, and a running Docker daemon.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-samples}"
OP_LOG="$(mktemp)"
OP_PID=""
SAMPLES=config/samples/tarantool3

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

# wait_sts <name> <want-ready> [timeout]
wait_sts() {
  local name="$1" want="$2" deadline=$(( SECONDS + ${3:-420} ))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(kubectl get statefulset "$name" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" = "$want" ] && return 0
    sleep 5
  done
  return 1
}

# run_sample <file> <sts:ready> [<sts:ready> ...] — apply, wait, delete.
run_sample() {
  local file="$1"; shift
  echo "==> sample: $file"
  kubectl apply -f "$SAMPLES/$file" >/dev/null
  for pair in "$@"; do
    local sts="${pair%%:*}" want="${pair##*:}"
    wait_sts "$sts" "$want" || fail "$file: StatefulSet $sts did not reach $want ready"
    echo "    $sts: $want ready"
  done
  kubectl delete -f "$SAMPLES/$file" --wait=true >/dev/null
  # Wait for the pods to actually terminate so samples don't pile up on the node.
  for pair in "$@"; do
    local sts="${pair%%:*}"
    for _ in $(seq 1 60); do
      kubectl get pods --no-headers 2>/dev/null | grep -q "^$sts-" || break
      sleep 3
    done
  done
  echo "    cleaned up"
}

for bin in kind kubectl go; do
  command -v "$bin" >/dev/null || { echo "missing required tool: $bin"; exit 1; }
done

echo "==> layer 1: render-validate every samples kustomization"
kubectl kustomize config/samples >/dev/null || fail "config/samples does not kustomize"
kubectl kustomize "$SAMPLES" >/dev/null || fail "tarantool3 samples do not kustomize"
kubectl kustomize config/samples/cartridge >/dev/null || fail "cartridge samples do not kustomize"
echo "    all kustomizations build"

echo "==> create kind cluster '$CLUSTER'"
kind create cluster --name "$CLUSTER" >/dev/null

echo "==> install CRDs"
kubectl apply -R -f config/crd/bases/ >/dev/null

echo "==> build and start operator (out-of-cluster)"
go build -o bin/manager .
./bin/manager --metrics-bind-address=0 --health-probe-bind-address=0 >"$OP_LOG" 2>&1 &
OP_PID=$!
for _ in $(seq 1 30); do
  grep -q 'Starting workers.*db.tarantool.io' "$OP_LOG" 2>/dev/null && break
  kill -0 "$OP_PID" 2>/dev/null || { echo "operator exited early:"; cat "$OP_LOG"; exit 1; }
  sleep 2
done

echo "==> layer 2: runtime-test the community-image-runnable samples"
run_sample db.tarantool.io_v2alpha1_minimal.yaml    minimal-storage:1
run_sample db.tarantool.io_v2alpha1_replicated.yaml replicated-storage:3
run_sample db.tarantool.io_v2alpha1_roles.yaml      approles-storage:1
run_sample db.tarantool.io_v2alpha1_advanced.yaml   advanced-router:3 advanced-storage-001:3 advanced-storage-002:3

echo "==> render-only samples (documented image/CRD requirements):"
echo "    - sharded trio: needs a vshard-bundled image"
echo "    - metrics: needs a metrics-export image + Prometheus Operator CRDs"

echo "PASS: samples e2e succeeded"

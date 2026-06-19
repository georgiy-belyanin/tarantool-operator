#!/usr/bin/env bash
#
# End-to-end test for configuration propagation: change cluster config parameters
# at runtime and verify every instance in the cluster picks up the new values
# (via box.cfg over the admin console socket).
#
# Deploys a two-instance replica set with initial passthrough config
# (memtx.memory, log.level), patches spec.config, and asserts both instances
# converge to the new values — exercising render -> Secret -> rollout -> live
# box.cfg across the whole replica set. Runs the operator out-of-cluster, same as
# the other e2e scripts.
#
# Usage:
#   test/e2e/kind-config.sh        # create, test, tear down
#   KEEP=1 test/e2e/kind-config.sh # leave the cluster up
#
# Requires: kind, kubectl, go, and a running Docker daemon.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-config}"
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

# box_uint <pod> <lua-expr>: print the first integer from box.cfg over the socket.
box_uint() {
  kubectl exec "$1" -- sh -c "echo 'return $2' | tt connect /var/run/tarantool/admin.socket 2>/dev/null" 2>/dev/null \
    | grep -oE '[0-9]+' | head -1
}

# wait_box <lua-expr> <expected> [timeout]: wait until BOTH instances report the
# expected value for the given box.cfg expression.
wait_box() {
  local expr="$1" want="$2" deadline=$(( SECONDS + ${3:-300} )) v0 v1
  while [ "$SECONDS" -lt "$deadline" ]; do
    v0="$(box_uint cfg-rs-0 "$expr")"
    v1="$(box_uint cfg-rs-1 "$expr")"
    [ "$v0" = "$want" ] && [ "$v1" = "$want" ] && return 0
    sleep 3
  done
  echo "    last seen: cfg-rs-0=$v0 cfg-rs-1=$v1 (want $want for $expr)"
  return 1
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

echo "==> apply cluster with initial config (memtx.memory=256MiB, log.level=5)"
kubectl apply -f test/e2e/testdata/config.yaml >/dev/null
for _ in $(seq 1 30); do
  kubectl get statefulset cfg-rs >/dev/null 2>&1 && break
  sleep 2
done
[ "$(kubectl get statefulset cfg-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" != "" ] || true

echo "==> assert initial config is live on all instances"
wait_box 'box.cfg.memtx_memory' 268435456 300 || fail "initial memtx.memory not applied"
wait_box 'box.cfg.log_level' 5 120 || fail "initial log.level not applied"
echo "    both instances: memtx_memory=256MiB, log_level=5"

echo "==> patch spec.config (memtx.memory=512MiB, log.level=6)"
kubectl patch cluster.db.tarantool.io cfg --type merge \
  -p '{"spec":{"config":{"memtx":{"memory":536870912},"log":{"level":6}}}}' >/dev/null

echo "==> assert the change propagated to every instance"
wait_box 'box.cfg.memtx_memory' 536870912 300 || fail "memtx.memory change did not propagate to all instances"
wait_box 'box.cfg.log_level' 6 300 || fail "log.level change did not propagate to all instances"
echo "    both instances: memtx_memory=512MiB, log_level=6"

echo "PASS: config-propagation e2e succeeded"

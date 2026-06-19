#!/usr/bin/env bash
#
# End-to-end test for instance scaling within a replica set: grow a replica set
# from 1 to 3 instances and shrink it back to 1, asserting at each step that the
# operator scales the StatefulSet, re-renders the cluster config (instances added
# / removed), and the instances reach a Ready (box.info: running) state.
#
# Uses manual failover with a fixed leader + persistent volumes so the topology
# is deterministic across the config-change rollout. Runs the operator
# out-of-cluster, same as kind-e2e.sh.
#
# Usage:
#   test/e2e/kind-scaling.sh        # create, test, tear down
#   KEEP=1 test/e2e/kind-scaling.sh # leave the cluster up
#
# Requires: kind, kubectl, go, and a running Docker daemon.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-scale}"
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

# wait_ready <statefulset> <want-ready> [timeout-seconds]
wait_ready() {
  local name="$1" want="$2" deadline=$(( SECONDS + ${3:-300} ))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(kubectl get statefulset "$name" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" = "$want" ] && return 0
    sleep 3
  done
  return 1
}

# wait_ready_instances <rs> <ready/total> <secs> : poll the ReplicaSet's
# status.readyInstances until it settles. On scale-up the existing pod also rolls
# (the RS subtree hash changes), so the count can dip after the StatefulSet first
# reports ready — poll rather than check once.
wait_ready_instances() {
  local name="$1" want="$2" deadline=$(( SECONDS + ${3:-180} ))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(kubectl get replicaset.db.tarantool.io "$name" -o jsonpath='{.status.readyInstances}' 2>/dev/null)" = "$want" ] && return 0
    sleep 3
  done
  return 1
}

# config_has <substring> : succeeds if the rendered config Secret contains it.
config_has() {
  kubectl get secret scale-config -o jsonpath='{.data.config\.yaml}' 2>/dev/null | base64 -d | grep -q "$1"
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

echo "==> apply cluster (replicas=1)"
kubectl apply -f test/e2e/testdata/scaling.yaml >/dev/null
for _ in $(seq 1 30); do
  kubectl get statefulset scale-rs >/dev/null 2>&1 && break
  sleep 2
done
wait_ready scale-rs 1 300 || fail "initial replica set did not reach 1/1"
echo "    initial 1/1 ready"

echo "==> scale UP to 3 replicas"
kubectl patch replicaset.db.tarantool.io scale-rs --type=merge -p '{"spec":{"replicas":3}}' >/dev/null
wait_ready scale-rs 3 300 || fail "scale-up did not reach 3/3 ready"
config_has 'scale-rs-2' || fail "rendered config does not include scale-rs-2 after scale-up"
wait_ready_instances scale-rs "3/3" 180 || fail "ReplicaSet status is not 3/3 after scale-up"
echo "    scaled up: 3/3 ready, config has the new instance, all instances joined"

echo "==> scale DOWN to 1 replica"
kubectl patch replicaset.db.tarantool.io scale-rs --type=merge -p '{"spec":{"replicas":1}}' >/dev/null
wait_ready scale-rs 1 300 || fail "scale-down did not reach 1/1 ready"
# The extra instance pods should be removed.
remaining=""
for _ in $(seq 1 40); do
  remaining="$(kubectl get pods -l tarantool.io/replicaset=scale-rs --no-headers 2>/dev/null | wc -l | tr -d ' ')"
  [ "$remaining" = "1" ] && break
  sleep 3
done
[ "$remaining" = "1" ] || fail "expected 1 instance pod after scale-down, found $remaining"
if config_has 'scale-rs-2'; then fail "rendered config still references scale-rs-2 after scale-down"; fi
wait_ready_instances scale-rs "1/1" 180 || fail "ReplicaSet status is not 1/1 after scale-down"
echo "    scaled down: 1/1 ready, config trimmed to one instance"

echo "==> assert removed instances are expelled, not orphaned (autoexpel, P4)"
# With replication.autoexpel, the surviving instance reconfigures and deletes the
# scaled-down instances from _cluster; box.info.replication must drop back to 1.
repl=""
deadline=$(( SECONDS + 180 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  repl="$(kubectl exec scale-rs-0 -- sh -c "echo 'return #box.info.replication' | tt connect /var/run/tarantool/admin.socket 2>/dev/null" 2>/dev/null | grep -oE '[0-9]+' | head -1)"
  [ "$repl" = "1" ] && break
  sleep 5
done
[ "$repl" = "1" ] || fail "box.info.replication=$repl after scale-down; removed instances were not expelled (autoexpel)"
echo "    autoexpel: box.info.replication back to 1 (no orphans)"

echo "PASS: scaling e2e succeeded"

#!/usr/bin/env bash
#
# End-to-end test for dead-node ("minion") remediation (REVIEW-FINAL F3, the 1C
# todo). A StatefulSet never replaces a pod on a lost node by itself; the operator
# must detect the stranded instance and force-delete it so it gets rescheduled.
#
# Uses a MULTI-NODE kind cluster (1 control plane + 3 workers). A 3-instance
# election replica set is spread across the workers; the test then STOPS the
# docker container of a worker hosting a follower and asserts:
#   * the operator force-deletes the stranded pod after the node-lost grace
#     (StuckPodRemediated event);
#   * the StatefulSet reschedules it onto a surviving worker (new UID, new node);
#   * the instance rejoins its replica set: 3/3 Ready and a survivor reports
#     #box.info.replication == 3 with the set writable.
#
# The operator runs out-of-cluster (remediation is API-only).
#
# Usage:
#   test/e2e/kind-nodeloss.sh
#   KEEP=1 test/e2e/kind-nodeloss.sh
#
# Requires: kind, kubectl, go, and a running Docker daemon.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-nodeloss}"
OP_LOG="$(mktemp)"
OP_PID=""
STOPPED_NODE=""

cleanup() {
  set +e
  [ -n "$OP_PID" ] && kill "$OP_PID" 2>/dev/null
  if [ -n "${KEEP:-}" ]; then
    echo "==> KEEP set: leaving kind cluster '$CLUSTER' running (operator stopped)"
  else
    echo "==> tearing down"
    [ -n "$STOPPED_NODE" ] && docker start "$STOPPED_NODE" >/dev/null 2>&1
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
  fi
  rm -f "$OP_LOG"
}
trap cleanup EXIT

fail() { echo "FAIL: $*"; kubectl get pods -o wide 2>/dev/null; kubectl get nodes 2>/dev/null; exit 1; }

tt_eval() {
  kubectl exec "$1" -- sh -c "echo '$2' | tt connect /var/run/tarantool/admin.socket 2>/dev/null" 2>/dev/null
}

for bin in kind kubectl go docker; do
  command -v "$bin" >/dev/null || { echo "missing required tool: $bin"; exit 1; }
done

echo "==> create multi-node kind cluster '$CLUSTER' (1 control plane + 4 workers)"
# 4 workers: required anti-affinity puts one instance per node, and after one
# worker is stopped the rescheduled replacement needs a free fourth node.
kind create cluster --name "$CLUSTER" --config - >/dev/null <<'YAML'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
  - role: worker
  - role: worker
YAML

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

echo "==> apply a 3-instance election cluster spread across workers"
kubectl apply -f test/e2e/testdata/nodeloss.yaml >/dev/null
deadline=$(( SECONDS + 600 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  [ "$(kubectl get statefulset nl-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" = "3" ] && break
  sleep 5
done
[ "$(kubectl get statefulset nl-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = "3" ] || fail "replica set did not reach 3/3 ready"
echo "    3/3 instances ready"
kubectl get pods -o wide | sed 's/^/    /'
# Sanity: required anti-affinity must have spread the instances 1-per-node.
distinct="$(kubectl get pods -l tarantool.io/replicaset=nl-rs -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' | sort -u | wc -l | tr -d ' ')"
[ "$distinct" = "3" ] || fail "instances not spread across 3 distinct nodes (got $distinct)"

echo "==> pick a follower and stop its worker node"
victim=""
for p in nl-rs-0 nl-rs-1 nl-rs-2; do
  ro="$(tt_eval "$p" 'return box.info.ro')"
  case "$ro" in *true*) victim="$p"; break ;; esac
done
[ -n "$victim" ] || fail "no follower found"
node="$(kubectl get pod "$victim" -o jsonpath='{.spec.nodeName}')"
uid="$(kubectl get pod "$victim" -o jsonpath='{.metadata.uid}')"
# Ensure another pod isn't on the same node as the victim (preferred anti-affinity
# usually spreads 3 pods over 3 workers; if not, this test still works — only the
# victim's identity is asserted).
echo "    victim=$victim on node=$node (uid ${uid:0:8})"
STOPPED_NODE="$node"
docker stop "$node" >/dev/null

echo "==> wait for remediation: pod force-deleted and rescheduled (new UID, different node)"
deadline=$(( SECONDS + 480 ))
newuid=""; newnode=""
while [ "$SECONDS" -lt "$deadline" ]; do
  newuid="$(kubectl get pod "$victim" -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
  newnode="$(kubectl get pod "$victim" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)"
  if [ -n "$newuid" ] && [ "$newuid" != "$uid" ] && [ -n "$newnode" ] && [ "$newnode" != "$node" ]; then
    break
  fi
  sleep 5
done
[ "$newuid" != "$uid" ] || fail "stranded pod was not replaced (still uid $uid) — remediation did not fire"
[ "$newnode" != "$node" ] || fail "replacement pod landed on the dead node ($newnode)"
echo "    replaced: new uid ${newuid:0:8} on node=$newnode"

echo "==> assert the StuckPodRemediated event was emitted"
kubectl get events --field-selector reason=StuckPodRemediated -o name 2>/dev/null | grep -q . \
  || fail "no StuckPodRemediated event found"

echo "==> assert the instance rejoins: 3/3 ready and replication intact"
deadline=$(( SECONDS + 480 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  [ "$(kubectl get statefulset nl-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" = "3" ] && break
  sleep 5
done
[ "$(kubectl get statefulset nl-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = "3" ] || fail "replica set did not return to 3/3 ready after remediation"
# A survivor must see all 3 members and the set must have a writable leader.
survivor=""
for p in nl-rs-0 nl-rs-1 nl-rs-2; do
  [ "$p" = "$victim" ] && continue
  survivor="$p"; break
done
repl=""
deadline=$(( SECONDS + 120 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  repl="$(tt_eval "$survivor" 'return #box.info.replication' | grep -oE '[0-9]+' | head -1)"
  [ "$repl" = "3" ] && break
  sleep 3
done
[ "$repl" = "3" ] || fail "survivor sees #box.info.replication=$repl, want 3 (rejoin failed)"
rw=0
for p in nl-rs-0 nl-rs-1 nl-rs-2; do
  ro="$(tt_eval "$p" 'return box.info.ro')"
  case "$ro" in *false*) rw=1 ;; esac
done
[ "$rw" = "1" ] || fail "no writable leader after remediation"
echo "    rejoined: 3/3 ready, replication=3, leader present"

echo "PASS: dead-node remediation e2e succeeded"

#!/usr/bin/env bash
#
# End-to-end test for the sharding scale-out / scale-in lifecycle on the
# operator: adding a storage replica set and letting vshard rebalance buckets
# onto it, then removing it (the operator drain-gates the delete: weight 0 + wait + finalizer release) — with no bucket loss at
# any step. Uses the tarantool-vshard image (bundles vshard).
#
# Story:
#   1. bring up a sharded cluster: router + storage-a + storage-b (weight 1
#      each), bucket_count 100; bootstrap vshard -> ~50/50;
#   2. ADD storage-c (weight 1) — the operator renders it into the config and
#      vshard rebalances to ~33/33/33 (total still 100);
#   3. SAFE REMOVE storage-c: delete the ReplicaSet directly (NO manual weight
#      0). The operator's drain finalizer holds the delete, renders weight 0,
#      waits for vshard to drain storage-c to 0, then releases the finalizer so
#      the StatefulSet is GC'd — a+b back to 100, no bucket lost.
#
# The operator runs IN-CLUSTER (sharding + bucket queries need iproto access).
#
# Usage:
#   test/e2e/kind-rebalance.sh
#   KEEP=1 test/e2e/kind-rebalance.sh
#
# Requires: kind, kubectl, go, and a running Docker daemon.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-rebalance}"
IMG="tarantool-operator:e2e"
TT_IMG="tarantool-vshard:3"
BUCKETS=100

cleanup() {
  set +e
  if [ -n "${KEEP:-}" ]; then
    echo "==> KEEP set: leaving kind cluster '$CLUSTER' running"
  else
    echo "==> tearing down"
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
  fi
}
trap cleanup EXIT

fail() { echo "FAIL: $*"; kubectl get pods,replicaset.db.tarantool.io 2>/dev/null; kubectl -n tt-operator logs deploy/tt-operator --tail=40 2>/dev/null; exit 1; }

tt_eval() {
  kubectl exec "$1" -- sh -c "echo '$2' | timeout 5 tt connect /var/run/tarantool/admin.socket 2>/dev/null" 2>/dev/null
}
# active buckets on a storage's instance
active() {
  local v i
  for i in 1 2 3 4 5; do
    v="$(tt_eval "$1" 'return require("vshard").storage.info().bucket.active' | grep -oE '[0-9]+' | head -1)"
    [ -n "$v" ] && { echo "$v"; return; }
    sleep 2
  done
  echo ""
}
# sum of active buckets across the given storage pods
total_active() { local s=0 v; for p in "$@"; do v="$(active "$p")"; s=$(( s + ${v:-0} )); done; echo "$s"; }

# wait_active <pod> <lo> <hi> <secs> — wait until pod's active buckets ∈ [lo,hi]
wait_active() {
  local pod="$1" lo="$2" hi="$3" deadline=$(( SECONDS + ${4:-180} )) v
  while [ "$SECONDS" -lt "$deadline" ]; do
    v="$(active "$pod")"; v="${v:-0}"
    if [ "$v" -ge "$lo" ] && [ "$v" -le "$hi" ]; then return 0; fi
    sleep 4
  done
  echo "    $pod active=$(active "$pod") (wanted [$lo,$hi])"
  return 1
}

for bin in kind kubectl go docker; do
  command -v "$bin" >/dev/null || { echo "missing required tool: $bin"; exit 1; }
done

echo "==> create kind cluster '$CLUSTER'"
kind create cluster --name "$CLUSTER" >/dev/null

echo "==> build images and load them into kind"
docker build -t "$IMG" . >/dev/null
docker build -t "$TT_IMG" docker/tarantool-vshard >/dev/null
kind load docker-image "$IMG" --name "$CLUSTER" >/dev/null
kind load docker-image "$TT_IMG" --name "$CLUSTER" >/dev/null

echo "==> install CRDs and deploy the operator in-cluster"
kubectl apply -R -f config/crd/bases/ >/dev/null
kubectl create namespace tt-operator >/dev/null
kubectl apply -f config/rbac/role.yaml >/dev/null
kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: { name: tt-operator, namespace: tt-operator }
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: { name: tt-operator }
roleRef: { apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: manager-role }
subjects:
  - { kind: ServiceAccount, name: tt-operator, namespace: tt-operator }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: tt-operator, namespace: tt-operator }
spec:
  replicas: 1
  selector: { matchLabels: { app: tt-operator } }
  template:
    metadata: { labels: { app: tt-operator } }
    spec:
      serviceAccountName: tt-operator
      containers:
        - name: manager
          image: ${IMG}
          imagePullPolicy: IfNotPresent
          args: ["--metrics-bind-address=0", "--health-probe-bind-address=0", "--leader-elect=false"]
YAML
kubectl -n tt-operator rollout status deploy/tt-operator --timeout=120s || fail "operator did not start"

echo "==> apply the sharded cluster (router + storage-a + storage-b)"
kubectl apply -f test/e2e/testdata/rebalance.yaml >/dev/null
for rs in router-a storage-a storage-b; do
  deadline=$(( SECONDS + 300 ))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(kubectl get statefulset "$rs" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" = "1" ] && break
    sleep 5
  done
  [ "$(kubectl get statefulset "$rs" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = "1" ] || fail "$rs did not become ready"
done

echo "==> the operator auto-bootstraps vshard (no manual bootstrap)"
# The operator runs vshard.router.bootstrap() through the router once the
# storages are reachable, then records it in status.shardingBootstrapped — so a
# sharded cluster distributes its buckets with no manual step.
deadline=$(( SECONDS + 180 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  [ "$(kubectl get replicaset.db.tarantool.io router-a -o jsonpath='{.status.shardingBootstrapped}' 2>/dev/null)" = "true" ] && break
  sleep 5
done
[ "$(kubectl get replicaset.db.tarantool.io router-a -o jsonpath='{.status.shardingBootstrapped}' 2>/dev/null)" = "true" ] \
  || fail "operator did not auto-bootstrap vshard (status.shardingBootstrapped not true)"
wait_active storage-a-0 30 70 180 || fail "storage-a did not receive buckets after bootstrap"
wait_active storage-b-0 30 70 180 || fail "storage-b did not receive buckets after bootstrap"
[ "$(total_active storage-a-0 storage-b-0)" = "$BUCKETS" ] || fail "bucket total != $BUCKETS after bootstrap (got $(total_active storage-a-0 storage-b-0))"
echo "    bootstrapped: a=$(active storage-a-0) b=$(active storage-b-0)"

echo "==> ADD storage-c (weight 1); vshard must rebalance onto it (~33 each)"
kubectl apply -f test/e2e/testdata/rebalance.add.yaml >/dev/null
deadline=$(( SECONDS + 300 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  [ "$(kubectl get statefulset storage-c -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" = "1" ] && break
  sleep 5
done
[ "$(kubectl get statefulset storage-c -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = "1" ] || fail "storage-c did not become ready"
wait_active storage-c-0 20 46 300 || fail "vshard did not rebalance buckets onto storage-c"
[ "$(total_active storage-a-0 storage-b-0 storage-c-0)" = "$BUCKETS" ] || fail "bucket total != $BUCKETS after scale-out (got $(total_active storage-a-0 storage-b-0 storage-c-0))"
echo "    rebalanced: a=$(active storage-a-0) b=$(active storage-b-0) c=$(active storage-c-0)"

echo "==> SAFE REMOVE storage-c: delete it directly (NO manual weight 0)"
# The operator's drain-gated finalizer must hold the delete, set weight 0, wait
# for vshard to drain storage-c, then release — so no buckets are lost. (A naive
# delete would drop storage-c's ~33 buckets and the total would fall below 100.)
kubectl delete replicaset.db.tarantool.io storage-c --wait=false >/dev/null
sleep 8
kubectl get replicaset.db.tarantool.io storage-c >/dev/null 2>&1 \
  || fail "storage-c vanished immediately — the drain finalizer did not engage"
fin="$(kubectl get replicaset.db.tarantool.io storage-c -o jsonpath='{.metadata.finalizers}' 2>/dev/null)"
echo "$fin" | grep -q 'drain-buckets' || fail "expected the drain-buckets finalizer to hold the delete (got: $fin)"
echo "    delete is held by the drain finalizer; vshard draining storage-c..."

# storage-c's pod stays up (finalizer holds the RS -> StatefulSet) while it drains.
wait_active storage-c-0 0 0 300 || fail "storage-c did not drain to 0 buckets under the finalizer"

echo "==> finalizer releases; StatefulSet is garbage-collected; no bucket lost"
deadline=$(( SECONDS + 120 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  kubectl get statefulset storage-c >/dev/null 2>&1 || break
  sleep 4
done
kubectl get statefulset storage-c >/dev/null 2>&1 && fail "storage-c StatefulSet was not garbage-collected after drain"
[ "$(total_active storage-a-0 storage-b-0)" = "$BUCKETS" ] || fail "bucket total != $BUCKETS after safe removal (got $(total_active storage-a-0 storage-b-0)) — data lost!"
echo "    removed safely: a=$(active storage-a-0) b=$(active storage-b-0); total still $BUCKETS"

echo "PASS: rebalance e2e succeeded (add -> rebalance -> safe drain-gated remove, no bucket loss)"

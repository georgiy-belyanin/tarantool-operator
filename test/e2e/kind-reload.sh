#!/usr/bin/env bash
#
# End-to-end test for operator-driven config HOT-RELOAD (REVIEW-FINAL F2). Dynamic
# config changes (memtx.memory, log.level) must apply to every instance WITHOUT a
# pod restart: the operator pushes the new config Secret and then calls
# config:reload() on each instance over iproto (it needs a "super" credential user
# and in-cluster network to the pods — so, like the leader e2e, the operator runs
# IN-CLUSTER here).
#
# Asserts:
#   * the new memtx.memory / log.level become live on all instances (box.cfg);
#   * the pods are NOT recreated (same pod UIDs) and the StatefulSet does NOT roll
#     (a single controller revision) — i.e. the change was hot-reloaded, not rolled.
#
# Usage:
#   test/e2e/kind-reload.sh
#   KEEP=1 test/e2e/kind-reload.sh
#
# Requires: kind, kubectl, go, and a running Docker daemon.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-reload}"
IMG="tarantool-operator:e2e"

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

fail() { echo "FAIL: $*"; kubectl get pods,sts,replicaset.db.tarantool.io 2>/dev/null; kubectl -n tt-operator logs deploy/tt-operator --tail=40 2>/dev/null; exit 1; }

INSTANCES="rel-rs-0 rel-rs-1 rel-rs-2"

# box_uint <pod> <lua-expr>: first integer from box.cfg over the admin console.
box_uint() {
  kubectl exec "$1" -- sh -c "echo 'return $2' | tt connect /var/run/tarantool/admin.socket 2>/dev/null" 2>/dev/null \
    | grep -oE '[0-9]+' | head -1
}

# wait_all_box <lua-expr> <want> <timeout>: wait until every instance reports want.
wait_all_box() {
  local expr="$1" want="$2" deadline=$(( SECONDS + ${3:-240} )) ok v
  while [ "$SECONDS" -lt "$deadline" ]; do
    ok=1
    for p in $INSTANCES; do
      v="$(box_uint "$p" "$expr")"
      [ "$v" = "$want" ] || ok=0
    done
    [ "$ok" = 1 ] && return 0
    sleep 3
  done
  echo "    last: $(for p in $INSTANCES; do printf '%s=%s ' "$p" "$(box_uint "$p" "$expr")"; done)(want $want)"
  return 1
}

pod_uids() { for p in $INSTANCES; do kubectl get pod "$p" -o jsonpath='{.metadata.uid}' 2>/dev/null; echo; done; }
revisions() { kubectl get controllerrevisions -l tarantool.io/replicaset=rel-rs -o name 2>/dev/null | wc -l | tr -d ' '; }

for bin in kind kubectl go docker; do
  command -v "$bin" >/dev/null || { echo "missing required tool: $bin"; exit 1; }
done

echo "==> create kind cluster '$CLUSTER'"
kind create cluster --name "$CLUSTER" >/dev/null

echo "==> build operator image and load it into kind"
docker build -t "$IMG" . >/dev/null
kind load docker-image "$IMG" --name "$CLUSTER" >/dev/null

echo "==> install CRDs"
kubectl apply -R -f config/crd/bases/ >/dev/null

echo "==> deploy the operator in-cluster"
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
kubectl -n tt-operator rollout status deploy/tt-operator --timeout=120s || fail "operator did not start in-cluster"

echo "==> apply a 3-instance election cluster (super user; memtx=256MiB, log.level=5)"
kubectl apply -f test/e2e/testdata/reload.yaml >/dev/null
for _ in $(seq 1 48); do
  [ "$(kubectl get statefulset rel-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" = "3" ] && break
  sleep 5
done
[ "$(kubectl get statefulset rel-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = "3" ] || fail "replica set did not reach 3/3 ready"
echo "    3/3 instances ready"

echo "==> assert initial config is live"
wait_all_box 'box.cfg.memtx_memory' 268435456 180 || fail "initial memtx.memory not live"
wait_all_box 'box.cfg.log_level' 5 60 || fail "initial log.level not live"

echo "==> record pod identities + revision count before the change"
before_uids="$(pod_uids)"
before_revs="$(revisions)"
echo "    revisions=$before_revs"

echo "==> patch dynamic config (memtx.memory=512MiB, log.level=6)"
kubectl patch cluster.db.tarantool.io rel --type merge \
  -p '{"spec":{"config":{"memtx":{"memory":536870912},"log":{"level":6}}}}' >/dev/null

echo "==> assert the change is HOT-RELOADED live on every instance"
wait_all_box 'box.cfg.memtx_memory' 536870912 240 || fail "memtx.memory change did not reload onto all instances"
wait_all_box 'box.cfg.log_level' 6 120 || fail "log.level change did not reload onto all instances"
echo "    all instances: memtx_memory=512MiB, log_level=6"

echo "==> assert NO pod restart and NO StatefulSet roll"
after_uids="$(pod_uids)"
after_revs="$(revisions)"
[ "$before_uids" = "$after_uids" ] || fail "pods were recreated (UIDs changed) — config change rolled instead of hot-reloading"$'\n'"before:"$'\n'"$before_uids"$'\n'"after:"$'\n'"$after_uids"
[ "$before_revs" = "$after_revs" ] || fail "StatefulSet rolled (controller revisions $before_revs -> $after_revs) — expected a hot reload"
echo "    same pod UIDs, revisions still $after_revs — hot-reloaded without a restart"

echo "PASS: config hot-reload e2e succeeded"

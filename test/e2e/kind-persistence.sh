#!/usr/bin/env bash
#
# End-to-end test for data persistence on PersistentVolumes. Uses a
# SINGLE-instance replica set so every recovery provably comes from the volume
# (snapshot load + WAL replay), never from a replication resync, then covers
# the volume lifecycle end to end:
#
#   1. pod kill            -> data survives, including a row written AFTER the
#                             last box.snapshot() (exercises xlog replay);
#   2. config rollout      -> a static config change rolls the pod; data intact;
#   3. CR delete/recreate  -> PVCs are retained (whenDeleted: Retain) and the
#                             recreated instance reattaches its old data;
#   4. scale 1->3->1->3    -> fresh volumes join and replicate; reload-driven
#                             scale-in expels WITHOUT restarting the survivor;
#                             scaling back up onto the expelled instances'
#                             RETAINED volumes leaves them orphaned (running
#                             and Ready but rejected by the leader — a known
#                             limitation: an expelled UUID cannot rejoin);
#                             deleting the stale PVCs is the documented remedy
#                             and makes them join fresh and replicate.
#
# The operator runs IN-cluster (like kind-reload.sh): hot reload and the
# expel path need pod-network iproto access.
#
# Usage:
#   test/e2e/kind-persistence.sh        # create, test, tear down
#   KEEP=1 test/e2e/kind-persistence.sh # leave the cluster up
#
# Env overrides (mostly for restricted environments):
#   IMG     operator image to deploy; built with `docker build .` unless
#           OP_PREBUILT=1, in which case IMG must already exist locally.
#   TT_IMG  tarantool image for the instances (must exist locally; it is
#           kind-loaded and substituted into the manifest).
#
# Requires: kind, kubectl, go, and a running Docker daemon.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-persist}"
IMG="${IMG:-tt-operator:e2e-persist}"

cleanup() {
  set +e
  if [ -n "${KEEP:-}" ]; then
    echo "==> KEEP set: leaving kind cluster '$CLUSTER' running"
  else
    echo "==> tearing down"
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
  fi
  rm -f "${MANIFEST:-}"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*"
  kubectl get pods,pvc,sts,replicaset.db.tarantool.io 2>/dev/null
  kubectl -n tt-operator logs deploy/tt-operator --tail=40 2>/dev/null
  exit 1
}

# tt_eval <pod> <lua> : evaluate Lua on the instance via its admin socket.
tt_eval() {
  kubectl exec "$1" -- sh -c "echo '$2' | tt connect /var/run/tarantool/admin.socket 2>/dev/null"
}

# wait_ready <statefulset> <want-ready> [timeout-seconds]
wait_ready() {
  local name="$1" want="$2" deadline=$(( SECONDS + ${3:-300} ))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(kubectl get statefulset "$name" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" = "$want" ] && return 0
    sleep 3
  done
  return 1
}

# wait_pod_replaced <pod> <old-uid> [timeout-seconds] : until the pod exists
# with a different UID and is ready.
wait_pod_replaced() {
  local pod="$1" old="$2" deadline=$(( SECONDS + ${3:-300} ))
  while [ "$SECONDS" -lt "$deadline" ]; do
    local uid ready
    uid="$(kubectl get pod "$pod" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
    ready="$(kubectl get pod "$pod" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null)"
    [ -n "$uid" ] && [ "$uid" != "$old" ] && [ "$ready" = "true" ] && return 0
    sleep 3
  done
  return 1
}

# leader_peers : number of box.info.replication entries on the leader.
leader_peers() {
  tt_eval persist-rs-0 'return #box.info.replication' 2>/dev/null | grep -oE '[0-9]+' | head -1
}

# wait_peers <want> [timeout-seconds]
wait_peers() {
  local want="$1" deadline=$(( SECONDS + ${2:-300} ))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(leader_peers)" = "$want" ] && return 0
    sleep 5
  done
  return 1
}

for bin in kind kubectl go docker; do
  command -v "$bin" >/dev/null || { echo "missing required tool: $bin"; exit 1; }
done

echo "==> create kind cluster '$CLUSTER'"
kind create cluster --name "$CLUSTER" >/dev/null

echo "==> build operator image and load it into kind"
if [ -z "${OP_PREBUILT:-}" ]; then
  docker build -t "$IMG" . >/dev/null
fi
kind load docker-image "$IMG" --name "$CLUSTER" >/dev/null

MANIFEST="$(mktemp)"
cp test/e2e/testdata/persistence.yaml "$MANIFEST"
if [ -n "${TT_IMG:-}" ]; then
  echo "==> use local tarantool image $TT_IMG"
  kind load docker-image "$TT_IMG" --name "$CLUSTER" >/dev/null
  sed -i.bak "s|tarantool/tarantool:3|$TT_IMG|" "$MANIFEST" && rm -f "$MANIFEST.bak"
fi

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

echo "==> apply single-instance cluster on a PersistentVolume"
kubectl apply -f "$MANIFEST" >/dev/null
wait_ready persist-rs 1 300 || fail "initial instance did not become ready"
kubectl get pvc data-persist-rs-0 >/dev/null || fail "PVC data-persist-rs-0 was not created"

echo "==> write data: one row covered by box.snapshot(), one only in the WAL"
tt_eval persist-rs-0 'box.schema.create_space("t"); box.space.t:create_index("pk"); box.space.t:insert{1, "snapshotted"}; box.snapshot(); box.space.t:insert{2, "wal-only"}; return box.space.t:len()' \
  | grep -q '^- 2' || fail "seed writes failed"

echo "==> 1) kill the pod; recovery must replay the WAL, not just the snapshot"
uid="$(kubectl get pod persist-rs-0 -o jsonpath='{.metadata.uid}')"
kubectl delete pod persist-rs-0 --wait=false >/dev/null
wait_pod_replaced persist-rs-0 "$uid" 300 || fail "pod was not recreated after delete"
tt_eval persist-rs-0 'return box.space.t:get(1)[2], box.space.t:get(2)[2]' | grep -q 'wal-only' \
  || fail "data lost after pod kill (WAL-only row missing)"
echo "    pod kill: snapshot + WAL rows recovered"

echo "==> 2) static config change rolls the pod; data intact"
uid="$(kubectl get pod persist-rs-0 -o jsonpath='{.metadata.uid}')"
kubectl patch cluster.db.tarantool.io persist --type=merge -p \
  '{"spec":{"config":{"credentials":{"users":{"replicator":{"roles":["replication"]}}},"replication":{"failover":"manual"},"iproto":{"threads":2}}}}' >/dev/null
wait_pod_replaced persist-rs-0 "$uid" 300 || fail "static config change did not roll the pod"
tt_eval persist-rs-0 'return box.space.t:len()' | grep -q '^- 2' || fail "data lost across the config rollout"
echo "    config rollout: data intact"

echo "==> 3) delete the ReplicaSet CR; PVC must be retained; recreate reattaches the data"
kubectl delete replicaset.db.tarantool.io persist-rs --wait=true --timeout=120s >/dev/null
phase="$(kubectl get pvc data-persist-rs-0 -o jsonpath='{.status.phase}' 2>/dev/null)"
[ "$phase" = "Bound" ] || fail "PVC was not retained after CR deletion (phase=$phase)"
kubectl apply -f "$MANIFEST" >/dev/null
wait_ready persist-rs 1 300 || fail "recreated instance did not become ready"
tt_eval persist-rs-0 'return box.space.t:get(2)[2]' | grep -q 'wal-only' \
  || fail "data lost across CR delete/recreate"
echo "    CR delete/recreate: PVC retained, data reattached"

echo "==> 4a) scale 1 -> 3: fresh volumes join and replicate"
kubectl patch replicaset.db.tarantool.io persist-rs --type=merge -p '{"spec":{"replicas":3}}' >/dev/null
wait_ready persist-rs 3 300 || fail "scale-up did not reach 3/3"
wait_peers 3 180 || fail "new instances did not register with the leader"
deadline=$(( SECONDS + 120 ))
until tt_eval persist-rs-2 'return box.space.t:len()' 2>/dev/null | grep -q '^- 2'; do
  [ "$SECONDS" -lt "$deadline" ] || fail "data did not replicate to the new instance"
  sleep 3
done
echo "    scale-up: 3 registered peers, data replicated"

echo "==> 4b) scale 3 -> 1: reload-driven expel must NOT restart the survivor"
uid="$(kubectl get pod persist-rs-0 -o jsonpath='{.metadata.uid}')"
kubectl patch replicaset.db.tarantool.io persist-rs --type=merge -p '{"spec":{"replicas":1}}' >/dev/null
wait_peers 1 300 || fail "scale-down did not expel the removed instances"
[ "$(kubectl get pod persist-rs-0 -o jsonpath='{.metadata.uid}')" = "$uid" ] \
  || fail "survivor was restarted on scale-down (expel should be applied via hot reload)"
kubectl get pvc data-persist-rs-1 >/dev/null || fail "scaled-away PVC should be retained (whenScaled: Retain)"
echo "    scale-down: expelled via hot reload, survivor untouched, PVCs retained"

echo "==> 4c) scale 1 -> 3 onto the RETAINED (stale) volumes: known limitation"
# The retained volumes belong to EXPELLED instances; an expelled UUID cannot
# rejoin, so the pods come up (and even pass readiness — box.info.status is
# "running") but the leader never registers them. Assert the limitation's
# observable shape so a behavior change (e.g. operator-side stale-volume
# handling) shows up here.
kubectl patch replicaset.db.tarantool.io persist-rs --type=merge -p '{"spec":{"replicas":3}}' >/dev/null
deadline=$(( SECONDS + 180 ))
until [ "$(kubectl get pod persist-rs-1 -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ]; do
  [ "$SECONDS" -lt "$deadline" ] || fail "scaled-up pod did not start"
  sleep 3
done
sleep 30 # give the appliers time; they must still be rejected
[ "$(leader_peers)" = "1" ] || fail "expected orphaned instances on stale volumes (leader registered them?)"
echo "    stale volumes: instances run but stay unregistered (documented limitation)"

echo "==> 4d) remedy: delete the stale PVCs; instances join fresh and replicate"
kubectl delete pvc data-persist-rs-1 data-persist-rs-2 --wait=false >/dev/null
kubectl delete pod persist-rs-1 persist-rs-2 --wait=false >/dev/null
wait_peers 3 300 || fail "instances did not rejoin after the stale PVCs were removed"
deadline=$(( SECONDS + 120 ))
until tt_eval persist-rs-1 'return box.space.t:get(1)[2]' 2>/dev/null | grep -q 'snapshotted'; do
  [ "$SECONDS" -lt "$deadline" ] || fail "data did not replicate to the rebuilt instance"
  sleep 3
done
echo "    remedy: fresh volumes joined and replicated"

echo "PASS: persistence e2e succeeded (WAL replay, rollout, CR recreate, expel round trip)"

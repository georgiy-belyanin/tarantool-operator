#!/usr/bin/env bash
#
# End-to-end test for leader observation (PROBLEMS.md P10). Unlike the other e2e
# scripts, the operator runs IN-CLUSTER here, because reading the elected leader
# over iproto (go-tarantool, box.info.election) requires reaching instance pods by
# their cluster DNS names — which an out-of-cluster operator cannot do.
#
# It builds the operator image, loads it into kind, deploys it with a minimal
# Deployment bound to the generated manager ClusterRole, then:
#   * asserts ReplicaSet.status.leader is populated (the operator observed the
#     elected leader over iproto);
#   * deletes the leader pod and asserts status.leader is re-observed (the
#     3-instance replica set keeps quorum and re-elects).
#
# Usage:
#   test/e2e/kind-leader.sh
#   KEEP=1 test/e2e/kind-leader.sh
#
# Requires: kind, kubectl, go, and a running Docker daemon.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-leader}"
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

fail() { echo "FAIL: $*"; kubectl get pods,replicaset.db.tarantool.io 2>/dev/null; kubectl -n tt-operator logs deploy/tt-operator --tail=30 2>/dev/null; exit 1; }

# status_leader prints ReplicaSet.status.leader.
status_leader() { kubectl get replicaset.db.tarantool.io leader-rs -o jsonpath='{.status.leader}' 2>/dev/null; }

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

echo "==> deploy the operator in-cluster (minimal Deployment + generated ClusterRole)"
kubectl create namespace tt-operator >/dev/null
kubectl apply -f config/rbac/role.yaml >/dev/null # ClusterRole "manager-role"
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

echo "==> apply a 3-instance election cluster"
kubectl apply -f test/e2e/testdata/leader.yaml >/dev/null
for _ in $(seq 1 40); do
  [ "$(kubectl get statefulset leader-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" = "3" ] && break
  sleep 5
done
[ "$(kubectl get statefulset leader-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = "3" ] || fail "replica set did not reach 3/3 ready"
echo "    3/3 instances ready"

echo "==> assert status.leader is observed over iproto"
leader=""
for _ in $(seq 1 40); do
  leader="$(status_leader)"
  [ -n "$leader" ] && break
  sleep 3
done
[ -n "$leader" ] || fail "status.leader stayed empty (operator did not observe the elected leader over iproto)"
echo "    observed leader: $leader"

echo "==> delete the leader pod; the replica set keeps quorum and re-elects"
kubectl delete pod "$leader" --wait=false >/dev/null
newLeader=""
for _ in $(seq 1 60); do
  newLeader="$(status_leader)"
  # Re-observed and stable (the deleted pod is recreated and may or may not regain
  # leadership); we just require a non-empty, currently-valid leader.
  if [ -n "$newLeader" ] && kubectl get pod "$newLeader" >/dev/null 2>&1; then
    ready="$(kubectl get pod "$newLeader" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null)"
    [ "$ready" = "true" ] && break
  fi
  sleep 3
done
[ -n "$newLeader" ] || fail "status.leader was not re-observed after leader pod deletion"
kubectl get pod "$newLeader" >/dev/null 2>&1 || fail "status.leader points at a non-existent pod: $newLeader"
echo "    re-observed leader after failover: $newLeader"

echo "PASS: leader-observation e2e succeeded"

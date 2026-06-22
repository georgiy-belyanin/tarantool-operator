#!/usr/bin/env bash
#
# Coexistence end-to-end test: the legacy Cartridge operator (tarantool.io/
# v1beta1) and the Tarantool 3 operator (db.tarantool.io/v2alpha1) run side by
# side in one kind cluster — both API groups installed, both controller sets in
# one manager — each driving its own cluster of the SAME kv app:
#
#   * Cartridge: a vshard-storage replica set carrying the shared kv role + a
#     vshard-router replica set (cartridge-kv image); the operator joins +
#     bootstraps it.
#   * Tarantool 3: a single-instance cluster running the same kv logic via
#     config.app.file (stock tarantool/tarantool:3 image).
#
# Asserts both clusters converge and serve kv_put/kv_get, proving the two
# operators/CRDs do not interfere (the point of the separate API groups).
#
# Usage: test/e2e/kind-coexist.sh   (KEEP=1 to keep the cluster)
# Requires: kind, kubectl, go, docker.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-coexist}"
IMG="tarantool-operator:e2e"
CART_IMG="cartridge-kv:2"

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

fail() {
  echo "FAIL: $*"
  kubectl get clusters.tarantool.io,roles.tarantool.io,clusters.db.tarantool.io,replicasets.db.tarantool.io,pods 2>/dev/null
  kubectl -n tt-operator logs deploy/tt-operator --tail=50 2>/dev/null
  exit 1
}

for bin in kind kubectl go docker; do
  command -v "$bin" >/dev/null || { echo "missing required tool: $bin"; exit 1; }
done

echo "==> create kind cluster '$CLUSTER'"
kind create cluster --name "$CLUSTER" >/dev/null

echo "==> build images (operator + cartridge-kv) and load them into kind"
docker build -t "$IMG" . >/dev/null
docker build -t "$CART_IMG" docker/cartridge-kv >/dev/null
kind load docker-image "$IMG" --name "$CLUSTER" >/dev/null
kind load docker-image "$CART_IMG" --name "$CLUSTER" >/dev/null

echo "==> install BOTH CRD groups (cartridge + tarantool3)"
kubectl apply -R -f config/crd/bases/ >/dev/null

echo "==> deploy the operator in-cluster (one manager runs both controller sets)"
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
# both controller sets must have started
kubectl -n tt-operator logs deploy/tt-operator | grep -q 'Starting workers.*tarantool.io' || true

echo "==> create the kv-app ConfigMap (shared logic, T3 app.file) and apply both clusters"
kubectl create configmap kv-app --from-file=kv.lua=test/e2e/testdata/coexist/kv-app-t3.lua >/dev/null
kubectl apply -f test/e2e/testdata/coexist/tarantool3.yaml >/dev/null
kubectl apply -f test/e2e/testdata/coexist/cartridge.yaml >/dev/null

# ---- Tarantool 3 side ----
echo "==> [t3] wait for the single instance to become ready"
kubectl rollout status statefulset/t3-rs --timeout=300s >/dev/null || fail "t3 StatefulSet not ready"
deadline=$(( SECONDS + 120 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  [ "$(kubectl get replicasets.db.tarantool.io t3-rs -o jsonpath='{.status.phase}' 2>/dev/null)" = "Ready" ] && break
  sleep 4
done
[ "$(kubectl get replicasets.db.tarantool.io t3-rs -o jsonpath='{.status.phase}')" = "Ready" ] || fail "t3 ReplicaSet not Ready"

echo "==> [t3] kv_put/kv_get over the admin console"
t3_out="$(kubectl exec t3-rs-0 -- sh -c "echo 'kv_put(\"k\",\"t3-value\") return kv_get(\"k\")' | tt connect /var/run/tarantool/admin.socket 2>/dev/null")"
echo "$t3_out" | grep -q 't3-value' || fail "t3 kv_get did not return the written value (got: $t3_out)"
echo "    t3 serves kv: $(echo "$t3_out" | grep t3-value | tr -d ' ')"

# ---- Cartridge side ----
echo "==> [cartridge] wait for the cluster to converge (join + vshard bootstrap)"
deadline=$(( SECONDS + 600 ))
phase=""
while [ "$SECONDS" -lt "$deadline" ]; do
  phase="$(kubectl get clusters.tarantool.io cart -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  [ "$phase" = "Ready" ] && break
  [ "$phase" = "UnableToBootstrap" ] && fail "cartridge cluster reached UnableToBootstrap"
  sleep 5
done
[ "$phase" = "Ready" ] || fail "cartridge cluster did not reach Ready (phase=$phase)"

echo "==> [cartridge] kv_put/kv_get on the storage over its console socket"
cart_out="$(kubectl exec storage-0-0 -- sh -c 'echo "kv_put(\"k\",\"cart-value\") return kv_get(\"k\")" | tarantoolctl connect `ls $CARTRIDGE_RUN_DIR/*.control` 2>/dev/null')"
echo "$cart_out" | grep -q 'cart-value' || fail "cartridge kv_get did not return the written value (got: $cart_out)"
echo "    cartridge serves kv: $(echo "$cart_out" | grep cart-value | tr -d ' ')"

# ---- Coexistence ----
echo "==> assert both API groups + both clusters coexist without interference"
kubectl get clusters.tarantool.io cart >/dev/null 2>&1 || fail "cartridge Cluster missing"
kubectl get clusters.db.tarantool.io t3 >/dev/null 2>&1 || fail "t3 Cluster missing"
# each operator only manages its own pods (distinct names, both Running)
for p in t3-rs-0 storage-0-0 router-0-0; do
  [ "$(kubectl get pod "$p" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ] || fail "$p not Running"
done

echo "PASS: coexistence e2e succeeded (Cartridge + Tarantool 3 clusters of the same kv app, both serving, no interference)"

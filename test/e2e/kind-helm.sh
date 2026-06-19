#!/usr/bin/env bash
#
# End-to-end test for the tarantool3-operator Helm chart: install the chart
# into a kind cluster and prove the deployment it produces actually operates a
# Tarantool 3 cluster — i.e. the chart's CRDs, RBAC, ServiceAccount and
# Deployment wiring are sufficient for the real controllers, not just
# renderable. Asserts:
#
#   1. helm install: CRDs land, the operator Deployment becomes ready with
#      leader election across its 2 replicas (exercising the leader-election
#      Role/Lease RBAC);
#   2. a Cluster + ReplicaSet reach Ready under the chart-deployed operator —
#      config Secret, built-in operator-user Secret, headless Service,
#      StatefulSet (core + apps RBAC);
#   3. a dynamic config change hot-reloads through the chart-deployed operator
#      without a pod restart (in-cluster iproto + secrets read + events RBAC);
#   4. helm upgrade is a clean no-op rollout;
#   5. helm uninstall removes the operator but leaves the data plane running
#      (CRDs and CRs stay, per Helm crds/ semantics — the database must not
#      depend on the operator's presence).
#
# Reuses testdata/persistence.yaml for the workload (single instance on a PV).
#
# Usage:
#   test/e2e/kind-helm.sh        # create, test, tear down
#   KEEP=1 test/e2e/kind-helm.sh # leave the cluster up
#
# Env overrides (mostly for restricted environments):
#   IMG          operator image; built with `docker build .` unless OP_PREBUILT=1.
#   TT_IMG       local tarantool image to kind-load and substitute into the manifest.
#   HELM         helm binary (default: helm).
#
# Requires: kind, kubectl, helm, go, and a running Docker daemon.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-helm}"
IMG="${IMG:-tt-operator:e2e-helm}"
HELM="${HELM:-helm}"
RELEASE="tt-op"
NS="tarantool-operator"
CHART="helm-charts/charts/tarantool3-operator"
# include "tarantool3-operator.fullname": <release>-<chart name> when the
# release name does not contain the chart name.
DEPLOY="${RELEASE}-tarantool3-operator"

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
  kubectl get pods,pvc,sts,replicaset.db.tarantool.io -A 2>/dev/null | head -30
  kubectl -n "$NS" logs "deploy/$DEPLOY" --tail=40 2>/dev/null
  exit 1
}

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

for bin in kind kubectl go docker "$HELM"; do
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

echo "==> 1) helm install the operator chart"
"$HELM" install "$RELEASE" "$CHART" \
  --namespace "$NS" --create-namespace \
  --set image.repository="${IMG%%:*}" \
  --set image.tag="${IMG##*:}" >/dev/null
kubectl get crd clusters.db.tarantool.io replicasets.db.tarantool.io >/dev/null \
  || fail "chart did not install the db.tarantool.io CRDs"
kubectl -n "$NS" rollout status "deploy/$DEPLOY" --timeout=180s \
  || fail "operator deployment did not become ready"
# Two replicas with leader election: exactly one acquires the Lease.
kubectl -n "$NS" get lease 0f93ee2c.tarantool.io -o jsonpath='{.spec.holderIdentity}' | grep -q . \
  || fail "no leader-election Lease holder (leader-election RBAC broken?)"
echo "    chart installed; operator ready; leader elected"

echo "==> 2) the chart-deployed operator brings a cluster to Ready"
kubectl apply -f "$MANIFEST" >/dev/null
wait_ready persist-rs 1 300 || fail "instance did not become ready under the chart-deployed operator"
kubectl get secret persist-operator-user >/dev/null \
  || fail "built-in operator-user Secret was not created (secrets RBAC broken?)"
deadline=$(( SECONDS + 120 ))
until kubectl get cluster.db.tarantool.io persist \
    -o jsonpath='{.status.conditions[?(@.type=="MembersAvailable")].status}' 2>/dev/null | grep -q True; do
  [ "$SECONDS" -lt "$deadline" ] || fail "Cluster never reported MembersAvailable=True"
  sleep 3
done
echo "    cluster Ready: config + operator-user Secrets, MembersAvailable=True"

echo "==> 3) hot reload works through the chart-deployed operator"
uid="$(kubectl get pod persist-rs-0 -o jsonpath='{.metadata.uid}')"
kubectl patch cluster.db.tarantool.io persist --type=merge -p \
  '{"spec":{"config":{"credentials":{"users":{"replicator":{"roles":["replication"]}}},"replication":{"failover":"manual"},"log":{"level":"debug"}}}}' >/dev/null
deadline=$(( SECONDS + 180 ))
until tt_eval persist-rs-0 "return require(\"config\"):get(\"log.level\")" 2>/dev/null | grep -q debug; do
  [ "$SECONDS" -lt "$deadline" ] || fail "dynamic config change was not hot-reloaded"
  sleep 4
done
[ "$(kubectl get pod persist-rs-0 -o jsonpath='{.metadata.uid}')" = "$uid" ] \
  || fail "pod restarted on a dynamic-only change (hot reload should apply it)"
echo "    log.level reloaded live, pod not restarted"

echo "==> 4) helm upgrade is a clean rollout"
"$HELM" upgrade "$RELEASE" "$CHART" --namespace "$NS" --reuse-values >/dev/null
kubectl -n "$NS" rollout status "deploy/$DEPLOY" --timeout=180s \
  || fail "operator deployment unhealthy after helm upgrade"
echo "    upgrade rolled out"

echo "==> 5) helm uninstall removes the operator, not the data plane"
"$HELM" uninstall "$RELEASE" --namespace "$NS" >/dev/null
deadline=$(( SECONDS + 60 ))
until ! kubectl -n "$NS" get "deploy/$DEPLOY" >/dev/null 2>&1; do
  [ "$SECONDS" -lt "$deadline" ] || fail "operator deployment survived helm uninstall"
  sleep 3
done
kubectl get crd clusters.db.tarantool.io >/dev/null || fail "CRDs must survive uninstall (helm crds/ semantics)"
kubectl get replicaset.db.tarantool.io persist-rs >/dev/null || fail "CRs must survive uninstall"
ready="$(kubectl get pod persist-rs-0 -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null)"
[ "$ready" = "true" ] || fail "the database must keep serving without the operator"
echo "    operator gone; CRDs, CRs and the running instance remain"

echo "PASS: helm chart e2e succeeded (install, operate, hot reload, upgrade, uninstall)"

#!/usr/bin/env bash
#
# End-to-end test for binary-upgrade orchestration (REVIEW-FINAL F9). Brings up a
# 3-instance election replica set on tarantool 3.6 (persistent storage, super
# user), patches the image to 3.7, and asserts:
#   * the StatefulSet rolls all instances to the new binary and they rejoin;
#   * the operator then runs box.schema.upgrade() on the leader exactly once
#     (SchemaUpgraded event; status.schemaUpgradedForImage = new image);
#   * data written before the upgrade survives it.
#
# The operator runs IN-CLUSTER (the schema step needs iproto access to the pods).
#
# Usage:
#   test/e2e/kind-upgrade.sh
#   KEEP=1 test/e2e/kind-upgrade.sh
#
# Requires: kind, kubectl, go, and a running Docker daemon.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-upgrade}"
IMG="tarantool-operator:e2e"
OLD_TT="tarantool/tarantool:3.6"
NEW_TT="tarantool/tarantool:3.7"

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

fail() { echo "FAIL: $*"; kubectl get pods -o wide 2>/dev/null; kubectl -n tt-operator logs deploy/tt-operator --tail=40 2>/dev/null; exit 1; }

tt_eval() {
  kubectl exec "$1" -- sh -c "echo '$2' | tt connect /var/run/tarantool/admin.socket 2>/dev/null" 2>/dev/null
}

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
kubectl -n tt-operator rollout status deploy/tt-operator --timeout=120s || fail "operator did not start"

echo "==> apply a 3-instance election cluster on ${OLD_TT}"
kubectl apply -f test/e2e/testdata/upgrade.yaml >/dev/null
deadline=$(( SECONDS + 600 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  [ "$(kubectl get statefulset up-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" = "3" ] && break
  sleep 5
done
[ "$(kubectl get statefulset up-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = "3" ] || fail "replica set did not reach 3/3 on ${OLD_TT}"
v0="$(tt_eval up-rs-0 'return box.info.version' | grep -oE '3\.[0-9]+' | head -1)"
echo "    3/3 ready on tarantool $v0"
[ "$v0" = "3.6" ] || fail "expected to start on 3.6, got $v0"

echo "==> seed data on the leader (must survive the upgrade)"
leader=""
for p in up-rs-0 up-rs-1 up-rs-2; do
  case "$(tt_eval "$p" 'return box.info.ro')" in *false*) leader="$p"; break ;; esac
done
[ -n "$leader" ] || fail "no writable leader before upgrade"
tt_eval "$leader" 'box.schema.space.create("keep", {if_not_exists=true}):create_index("pk", {if_not_exists=true}) box.space.keep:replace({1, "survives"})' >/dev/null
echo "$(tt_eval "$leader" 'return box.space.keep:get(1)[2]')" | grep -q survives || fail "seed write failed"

echo "==> patch the image to ${NEW_TT}"
kubectl patch replicaset.db.tarantool.io up-rs --type json \
  -p '[{"op":"replace","path":"/spec/podTemplate/spec/containers/0/image","value":"'"$NEW_TT"'"}]' >/dev/null

echo "==> wait for the rollout to the new binary (3/3 ready on 3.7)"
deadline=$(( SECONDS + 900 ))
ok=0
while [ "$SECONDS" -lt "$deadline" ]; do
  ready="$(kubectl get statefulset up-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)"
  cur="$(kubectl get statefulset up-rs -o jsonpath='{.status.currentRevision}' 2>/dev/null)"
  upd="$(kubectl get statefulset up-rs -o jsonpath='{.status.updateRevision}' 2>/dev/null)"
  if [ "$ready" = "3" ] && [ -n "$cur" ] && [ "$cur" = "$upd" ]; then ok=1; break; fi
  sleep 5
done
[ "$ok" = 1 ] || fail "rollout to ${NEW_TT} did not converge"
for p in up-rs-0 up-rs-1 up-rs-2; do
  v="$(tt_eval "$p" 'return box.info.version' | grep -oE '3\.[0-9]+' | head -1)"
  [ "$v" = "3.7" ] || fail "$p still on $v after the rollout"
done
echo "    all instances on 3.7"

echo "==> assert the operator ran the schema upgrade (status + event)"
deadline=$(( SECONDS + 240 ))
got=""
while [ "$SECONDS" -lt "$deadline" ]; do
  got="$(kubectl get replicaset.db.tarantool.io up-rs -o jsonpath='{.status.schemaUpgradedForImage}' 2>/dev/null)"
  [ "$got" = "$NEW_TT" ] && break
  sleep 5
done
[ "$got" = "$NEW_TT" ] || fail "status.schemaUpgradedForImage=$got, want $NEW_TT (schema upgrade did not run)"
kubectl get events --field-selector reason=SchemaUpgraded -o name 2>/dev/null | grep -q . || fail "no SchemaUpgraded event"
echo "    schema upgraded for $NEW_TT (event present)"

echo "==> assert pre-upgrade data survived"
for p in up-rs-0 up-rs-1 up-rs-2; do
  case "$(tt_eval "$p" 'return box.info.ro')" in *false*) leader="$p" ;; esac
done
echo "$(tt_eval "$leader" 'return box.space.keep:get(1)[2]')" | grep -q survives || fail "pre-upgrade data lost"
echo "    data intact on the post-upgrade leader ($leader)"

echo "PASS: binary-upgrade e2e succeeded (3.6 -> 3.7, schema upgraded, data intact)"

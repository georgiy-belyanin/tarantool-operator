#!/usr/bin/env bash
#
# End-to-end test for ./deliver-role — the one-command "magic button" that ships a
# Tarantool 3 application role to a running cluster. It pushes the role's Lua as a
# ConfigMap and wires it into the ReplicaSet (mount + LUA_PATH + spec.config.roles);
# the operator applies it over config:reload(). On a STOCK tarantool/tarantool:3
# image this suite proves:
#
#   1. a 1-instance cluster comes up with NO role;
#   2. `deliver-role greeter-role.rpm -r dr-rs` extracts the role's .lua FROM AN
#      RPM, delivers + enables it, and its apply() runs (e2e_role_applied). This
#      FIRST delivery attaches the role mount, so the instance is recreated once —
#      expected. (The fixture rpm was made with `tt pack rpm`; see testdata.)
#   3. `deliver-role counter.lua -r dr-rs` (mount now present) delivers a SECOND
#      role from a plain .lua, HOT: its apply() runs with the SAME pod UID and
#      controller revision — no restart. This is the steady-state property: once a
#      replicaset carries roles, further deliveries (new modules, updated code,
#      another rpm) hot-reload.
#
# So this covers both inputs (.rpm and .lua) and the hot-reload property. The
# operator runs IN-CLUSTER (config:reload needs iproto + a super user, which the
# built-in operator user provides).
#
# Usage: test/e2e/kind-deliver-role.sh   [KEEP=1 to leave the cluster up]
# Requires: kind, kubectl, go, docker, python3, cpio.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-deliver-role}"
OPIMG="tarantool-operator:e2e"
POD="dr-rs-0"

cleanup() {
  set +e
  if [ -n "${KEEP:-}" ]; then echo "==> KEEP set: leaving kind cluster '$CLUSTER' running";
  else echo "==> tearing down"; kind delete cluster --name "$CLUSTER" >/dev/null 2>&1; fi
}
trap cleanup EXIT
fail() { echo "FAIL: $*"; kubectl get pods,replicaset.db.tarantool.io 2>/dev/null; kubectl -n tt-operator logs deploy/tt-operator --tail=30 2>/dev/null; exit 1; }
tt_eval() { kubectl exec "$POD" -- sh -c "echo '$1' | tt connect /var/run/tarantool/admin.socket 2>/dev/null" 2>/dev/null; }

for bin in kind kubectl go docker python3 cpio; do command -v "$bin" >/dev/null || { echo "missing required tool: $bin"; exit 1; }; done

echo "==> create kind cluster '$CLUSTER'"
kind create cluster --name "$CLUSTER" >/dev/null

echo "==> build + load the operator image"
docker build -t "$OPIMG" . >/dev/null
kind load docker-image "$OPIMG" --name "$CLUSTER" >/dev/null

echo "==> install CRDs + deploy the operator in-cluster"
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
subjects: [{ kind: ServiceAccount, name: tt-operator, namespace: tt-operator }]
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
          image: ${OPIMG}
          imagePullPolicy: IfNotPresent
          args: ["--metrics-bind-address=0", "--health-probe-bind-address=0", "--leader-elect=false"]
YAML
kubectl -n tt-operator rollout status deploy/tt-operator --timeout=120s || fail "operator did not start"

echo "==> apply a 1-instance cluster with NO role (stock tarantool image)"
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: db.tarantool.io/v2alpha1
kind: Cluster
metadata: { name: dr }
spec: {}
---
apiVersion: db.tarantool.io/v2alpha1
kind: ReplicaSet
metadata: { name: dr-rs }
spec:
  clusterName: dr
  replicas: 1
  podTemplate:
    spec:
      containers:
      - name: tarantool
        image: tarantool/tarantool:3
        ports: [{ name: iproto, containerPort: 3301 }]
YAML
for _ in $(seq 1 48); do [ "$(kubectl get statefulset dr-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" = "1" ] && break; sleep 5; done
[ "$(kubectl get statefulset dr-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = "1" ] || fail "cluster did not reach 1/1 ready"
echo "    1/1 ready"
tmp="$(mktemp -d)"
# greeter is delivered FROM AN RPM (fixture below); counter from a plain .lua.
printf 'return { validate=function() end, apply=function() rawset(_G,"e2e_counter_applied",true) end, stop=function() end, dependencies={} }\n' > "$tmp/counter.lua"

echo "==> magic button (1st role, from an RPM): ./deliver-role greeter-role.rpm -r dr-rs"
./deliver-role test/e2e/testdata/greeter-role.rpm -r dr-rs
ok=""
for _ in $(seq 1 60); do tt_eval 'return rawget(_G, "e2e_role_applied")' | grep -q true && { ok=1; break; }; sleep 4; done
[ -n "$ok" ] || fail "role delivered from the RPM did not apply (e2e_role_applied not true)"
kubectl exec "$POD" -- sh -c 'test -f /opt/roles/greeter.lua' || fail "role module (from the RPM) not mounted at /opt/roles"
# First delivery attaches the role mount, so the instance is recreated once;
# wait for it to settle and capture the (post-roll) identity to compare against.
for _ in $(seq 1 48); do [ "$(kubectl get statefulset dr-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" = "1" ] && break; sleep 5; done
[ "$(kubectl get statefulset dr-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = "1" ] || fail "instance did not return to ready after the first role delivery"
uid_mid="$(kubectl get pod "$POD" -o jsonpath='{.metadata.uid}')"
revs_mid="$(kubectl get controllerrevisions -l tarantool.io/replicaset=dr-rs -o name | wc -l | tr -d ' ')"
echo "    greeter applied (uid ${uid_mid:0:8}, $revs_mid revisions)"

echo "==> magic button (2nd role, mount present): ./deliver-role counter.lua -r dr-rs — expect HOT"
./deliver-role "$tmp/counter.lua" -r dr-rs
ok=""
for _ in $(seq 1 60); do tt_eval 'return rawget(_G, "e2e_counter_applied")' | grep -q true && { ok=1; break; }; sleep 4; done
[ -n "$ok" ] || fail "second delivered role did not apply (e2e_counter_applied not true)"
uid_after="$(kubectl get pod "$POD" -o jsonpath='{.metadata.uid}')"
revs_after="$(kubectl get controllerrevisions -l tarantool.io/replicaset=dr-rs -o name | wc -l | tr -d ' ')"
[ "$uid_after" = "$uid_mid" ] || fail "pod was recreated on an incremental delivery (uid ${uid_mid:0:8} -> ${uid_after:0:8}) — expected a hot reload"
[ "$revs_after" = "$revs_mid" ] || fail "StatefulSet rolled on an incremental delivery (revisions $revs_mid -> $revs_after) — expected a hot reload"
# greeter must still be applied too (config:reload re-applied all roles).
tt_eval 'return rawget(_G, "e2e_role_applied")' | grep -q true || fail "greeter stopped being applied after delivering counter"
echo "    counter applied; same pod UID and revision — hot-reloaded, no restart"

echo "PASS: deliver-role e2e succeeded (one command delivers + enables a role; incremental deliveries hot-reload)"

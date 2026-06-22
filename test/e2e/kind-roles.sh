#!/usr/bin/env bash
#
# End-to-end test for the application-roles mechanism (as opposed to app.file).
# A user role module is supplied from a ConfigMap, enabled via the operator's
# spec.roles and configured via spec.rolesCfg, and made requireable via LUA_PATH.
# The test then verifies over the admin console that the role's apply() ran,
# received its roles_cfg, and performed its data-plane action:
#   * e2e_role_applied marker is set (the role's apply() ran),
#   * the role received its config (greeting == "hello-from-role"),
#   * the role's data-plane logic stored the greeting in a space.
#
# Runs the operator out-of-cluster, same as the other e2e scripts.
#
# Usage:
#   test/e2e/kind-roles.sh        # create, test, tear down
#   KEEP=1 test/e2e/kind-roles.sh # leave the cluster up
#
# Requires: kind, kubectl, go, and a running Docker daemon.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-roles}"
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

fail() { echo "FAIL: $*"; kubectl get pods,sts 2>/dev/null; exit 1; }

# tt_eval <pod> <lua>: run a Lua expression over the admin console, print output.
tt_eval() {
  kubectl exec "$1" -- sh -c "echo '$2' | tt connect /var/run/tarantool/admin.socket 2>/dev/null" 2>/dev/null
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

echo "==> apply cluster with a user application role (spec.roles + spec.rolesCfg)"
kubectl apply -f test/e2e/testdata/roles.yaml >/dev/null
for _ in $(seq 1 30); do
  kubectl get statefulset roles-rs >/dev/null 2>&1 && break
  sleep 2
done
echo "==> wait for the instance to be Ready"
kubectl rollout status statefulset/roles-rs --timeout=240s || {
  kubectl describe pod roles-rs-0 2>/dev/null | tail -20
  fail "instance did not become ready"
}

echo "==> assert the operator rendered roles / roles_cfg into the config"
cfg="$(kubectl get secret roles-config -o jsonpath='{.data.config\.yaml}' | base64 -d)"
echo "$cfg" | grep -q 'greeter' || fail "config missing the 'greeter' role: $cfg"

echo "==> assert the role's apply() ran"
out="$(tt_eval roles-rs-0 'return rawget(_G, "e2e_role_applied")')"
echo "$out" | grep -q 'true' || fail "role apply() did not run (e2e_role_applied not true): $out"

echo "==> assert the role received its roles_cfg"
out="$(tt_eval roles-rs-0 'return rawget(_G, "e2e_role_greeting")')"
echo "$out" | grep -q 'hello-from-role' || fail "role did not receive its config greeting: $out"

echo "==> assert the role's data-plane logic stored data"
out="$(tt_eval roles-rs-0 'return box.space.greetings:get(1)[2]')"
echo "$out" | grep -q 'hello-from-role' || fail "role data-plane logic did not store the greeting: $out"

echo "PASS: application-roles e2e succeeded"

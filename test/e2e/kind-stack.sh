#!/usr/bin/env bash
#
# Full-stack end-to-end test: ONE cluster that combines what the smaller e2es
# exercise separately — a large, replicated topology PLUS an application role PLUS
# a user Lua app, and verifies that data written through the role and the app
# replicates to followers.
#
# Topology (test/e2e/testdata/stack.yaml): 9 instances, 2 groups, 3 replica sets,
# election failover. Replica sets are 3 instances so a fresh election set reaches
# quorum (2 of 3) and elects a leader reliably at bootstrap.
#   group "routers":  router-001                  (3 instances) — runs a user ROLE
#   group "storages": storage-001/002             (2 x 3 = 6)   — run a user APP (app.file)
#
# Verifies:
#   1. every instance Ready; each replica set replicates only within itself
#      (box.info.replication == 3, i.e. isolated and a leader was elected);
#   2. the operator rendered both groups + roles/roles_cfg + the storages' app.file;
#   3. the application ROLE on routers: apply() ran, received its roles_cfg, and a
#      row written by the role on the leader replicates to the follower;
#   4. the Lua APP on storages: app.file loaded, a user function computes, and a
#      row seeded on the leader replicates to the follower.
#
# Role/app writes are driven by the e2e on the writable leader (election decides
# leadership after bootstrap, so neither writes from inside apply()/load).
#
# Runs the operator out-of-cluster, same as the other e2e scripts.
#
# Usage:
#   test/e2e/kind-stack.sh        # create, test, tear down
#   KEEP=1 test/e2e/kind-stack.sh # leave the cluster up
#
# Requires: kind, kubectl, go, and a running Docker daemon. 9 Tarantool pods land
# on a single kind node, so the fixture sets a small memtx per instance.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-stack}"
OP_LOG="$(mktemp)"
OP_PID=""

REPLICASETS="router-001 storage-001 storage-002"
REPLICAS=3
WANT_INSTANCES=9

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

fail() { echo "FAIL: $*"; kubectl get pods,sts,replicaset.db.tarantool.io 2>/dev/null; exit 1; }

# tt_eval <pod> <lua>: run a Lua expression over the admin console, print output.
# The Lua must not contain single quotes (it is wrapped in echo '...').
tt_eval() {
  kubectl exec "$1" -- sh -c "echo '$2' | tt connect /var/run/tarantool/admin.socket 2>/dev/null" 2>/dev/null
}

# find_rw <rs> <count>: echo the first instance whose box.info.ro is false
# (the elected leader), retrying until one appears.
find_rw() {
  local rs="$1" count="$2" i out deadline=$(( SECONDS + 120 ))
  while [ "$SECONDS" -lt "$deadline" ]; do
    for i in $(seq 0 $((count - 1))); do
      out="$(tt_eval "${rs}-${i}" 'return box.info.ro')"
      case "$out" in *false*) echo "${rs}-${i}"; return 0 ;; esac
    done
    sleep 3
  done
  return 1
}

# find_ro <rs> <count>: echo the first instance whose box.info.ro is true
# (a follower), used to check that leader-written data replicated.
find_ro() {
  local rs="$1" count="$2" i out
  for i in $(seq 0 $((count - 1))); do
    out="$(tt_eval "${rs}-${i}" 'return box.info.ro')"
    case "$out" in *true*) echo "${rs}-${i}"; return 0 ;; esac
  done
  return 1
}

# wait_value <pod> <lua> <want-substr>: poll a Lua expression until its output
# contains want-substr (for eventually-consistent replication checks).
wait_value() {
  local pod="$1" lua="$2" want="$3" out deadline=$(( SECONDS + 120 ))
  while [ "$SECONDS" -lt "$deadline" ]; do
    out="$(tt_eval "$pod" "$lua")"
    echo "$out" | grep -q "$want" && return 0
    sleep 3
  done
  echo "$out"
  return 1
}

for bin in kind kubectl go; do
  command -v "$bin" >/dev/null || { echo "missing required tool: $bin"; exit 1; }
done

echo "==> create kind cluster '$CLUSTER'"
kind create cluster --name "$CLUSTER" >/dev/null

echo "==> install CRDs"
# -R so it works whether config/crd/bases is flat or split into per-API subdirs.
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

echo "==> apply full stack (2 groups, 3 replica sets, ${WANT_INSTANCES} instances, role + app)"
kubectl apply -f test/e2e/testdata/stack.yaml >/dev/null

echo "==> wait for all ${WANT_INSTANCES} instances to become Ready"
deadline=$(( SECONDS + 480 ))
total=0
while [ "$SECONDS" -lt "$deadline" ]; do
  total=0
  all_ready=1
  for rs in $REPLICASETS; do
    ready="$(kubectl get statefulset "$rs" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)"
    ready="${ready:-0}"
    total=$(( total + ready ))
    [ "$ready" = "$REPLICAS" ] || all_ready=0
  done
  [ "$all_ready" = "1" ] && break
  sleep 5
done
[ "$total" = "$WANT_INSTANCES" ] || fail "only $total/$WANT_INSTANCES instances Ready"
echo "    all ${WANT_INSTANCES} instances Ready across 3 replica sets"

echo "==> assert rendered config has both groups, the role, and the storages' app.file"
cfg="$(kubectl get secret stack-config -o jsonpath='{.data.config\.yaml}' | base64 -d)"
echo "$cfg" | grep -q 'routers:'        || fail "config missing 'routers' group"
echo "$cfg" | grep -q 'storages:'       || fail "config missing 'storages' group"
echo "$cfg" | grep -q 'greeter'         || fail "config missing the 'greeter' role / roles_cfg"
echo "$cfg" | grep -q '/app/init.lua'   || fail "config missing the storages' app.file"
echo "    config has 2 groups, the role, and app.file"

echo "==> assert each replica set replicates within itself (isolated 3-instance sets)"
for rs in router-001 storage-001; do
  repl=""
  d=$(( SECONDS + 120 ))
  while [ "$SECONDS" -lt "$d" ]; do
    repl="$(tt_eval "${rs}-0" 'return #box.info.replication' | grep -oE '[0-9]+' | head -1)"
    [ "$repl" = "$REPLICAS" ] && break
    sleep 3
  done
  [ "$repl" = "$REPLICAS" ] || fail "$rs should replicate within its ${REPLICAS}-instance set, box.info.replication=$repl"
done
echo "    router-001 and storage-001 each replicate within their own ${REPLICAS}-instance set"

echo "==> application ROLE (routers): apply() ran and received its config"
out="$(tt_eval router-001-0 'return rawget(_G, "e2e_role_applied")')"
echo "$out" | grep -q 'true' || fail "role apply() did not run on router-001-0: $out"
out="$(tt_eval router-001-0 'return rawget(_G, "e2e_role_greeting")')"
echo "$out" | grep -q 'hello-from-role' || fail "role did not receive its roles_cfg greeting: $out"

echo "==> application ROLE: seed on the leader, assert it replicates to a follower"
rleader="$(find_rw router-001 "$REPLICAS")" || fail "router-001 elected no writable leader"
out="$(tt_eval "$rleader" 'return e2e_role_seed()')"
echo "$out" | grep -q 'hello-from-role' || fail "role seed on leader $rleader failed: $out"
rfollower="$(find_ro router-001 "$REPLICAS")" || fail "router-001 has no follower"
wait_value "$rfollower" \
  'return (box.space.greetings ~= nil and box.space.greetings:get(1) ~= nil) and box.space.greetings:get(1)[2] or "none"' \
  'hello-from-role' \
  || fail "role-written row did not replicate to follower $rfollower"
echo "    role wrote on $rleader; replicated to $rfollower"

echo "==> Lua APP (storages): app.file loaded and a user function computes"
out="$(tt_eval storage-001-0 'return rawget(_G, "e2e_app_loaded")')"
echo "$out" | grep -q 'true' || fail "user app did not load on storage-001-0: $out"
out="$(tt_eval storage-001-0 'return e2e_sum(40, 2)')"
echo "$out" | grep -qE '(^|[^0-9])42([^0-9]|$)' || fail "user function e2e_sum(40,2) != 42: $out"

echo "==> Lua APP: seed on the leader, assert it replicates to a follower"
sleader="$(find_rw storage-001 "$REPLICAS")" || fail "storage-001 elected no writable leader"
out="$(tt_eval "$sleader" 'return e2e_seed()')"
echo "$out" | grep -q 'widget' || fail "app seed on leader $sleader failed: $out"
sfollower="$(find_ro storage-001 "$REPLICAS")" || fail "storage-001 has no follower"
wait_value "$sfollower" \
  'return (box.space.items ~= nil and box.space.items:get(1) ~= nil) and box.space.items:get(1)[2] or "none"' \
  'widget' \
  || fail "app-seeded row did not replicate to follower $sfollower"
echo "    app seeded on $sleader; replicated to $sfollower"

echo "PASS: full-stack e2e succeeded (${WANT_INSTANCES} instances, role + app, data replicated)"

#!/usr/bin/env bash
#
# End-to-end test for a large topology: ~10 instances across 2 groups and 5
# replica sets (1 router replica set + 4 storage replica sets, 2 instances each).
# Verifies the operator renders a multi-group / multi-replicaset config and brings
# every instance to Ready, and that the replica sets are independent (each
# instance replicates only within its own replica set).
#
# Runs the operator out-of-cluster, same as the other e2e scripts.
#
# Usage:
#   test/e2e/kind-large.sh        # create, test, tear down
#   KEEP=1 test/e2e/kind-large.sh # leave the cluster up
#
# Requires: kind, kubectl, go, and a running Docker daemon. ~10 Tarantool pods
# land on a single kind node, so the test sets a small memtx per instance.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-large}"
OP_LOG="$(mktemp)"
OP_PID=""

# All replica sets in test/e2e/testdata/large.yaml (each has 2 instances).
REPLICASETS="router-001 storage-001 storage-002 storage-003 storage-004"
WANT_INSTANCES=10

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

echo "==> apply large topology (2 groups, 5 replica sets, ${WANT_INSTANCES} instances)"
kubectl apply -f test/e2e/testdata/large.yaml >/dev/null

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
    [ "$ready" = "2" ] || all_ready=0
  done
  [ "$all_ready" = "1" ] && break
  sleep 5
done
[ "$total" = "$WANT_INSTANCES" ] || fail "only $total/$WANT_INSTANCES instances Ready"
echo "    all ${WANT_INSTANCES} instances Ready across 5 replica sets"

echo "==> assert rendered config has both groups and all replica sets/instances"
cfg="$(kubectl get secret big-config -o jsonpath='{.data.config\.yaml}' | base64 -d)"
echo "$cfg" | grep -q 'routers:' || fail "config missing 'routers' group"
echo "$cfg" | grep -q 'storages:' || fail "config missing 'storages' group"
for inst in router-001-0 router-001-1 storage-001-0 storage-004-1; do
  echo "$cfg" | grep -q "$inst" || fail "config missing instance $inst"
done
echo "    config has 2 groups and the expected instances"

echo "==> assert replica sets are independent (each instance replicates only within its replica set)"
# box.info.replication settles a few seconds after both pods report Ready (the
# replica registers in the leader's view), so poll until it reaches exactly 2 —
# which also proves the replica set is isolated (not all 10 instances in one).
repl=""
deadline=$(( SECONDS + 120 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  repl="$(kubectl exec storage-001-0 -- sh -c "echo 'return #box.info.replication' | tt connect /var/run/tarantool/admin.socket 2>/dev/null" 2>/dev/null | grep -oE '[0-9]+' | head -1)"
  [ "$repl" = "2" ] && break
  sleep 3
done
[ "$repl" = "2" ] || fail "storage-001 should replicate within its 2-instance replica set, box.info.replication=$repl"
echo "    storage-001 replicates within its own 2-instance replica set (isolated from the other 8 instances)"

echo "PASS: large-topology e2e succeeded (${WANT_INSTANCES} instances, 5 replica sets, 2 groups)"

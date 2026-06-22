#!/usr/bin/env bash
#
# End-to-end test for the Degraded phase: a previously-Ready replica set that
# loses an instance must report phase=Degraded (a regression), while a set that
# is still converging reports Configuring — never Degraded at startup.
#
# The degradation is the realistic nasty kind: the test wedges one instance's
# TX thread with an infinite Lua loop (`while true do end` over the admin
# console). The process stays alive (pod Running) but stops answering anything —
# the readiness probe (box.info.status over the same console) fails, the pod
# goes 0/1, and the operator must call that Degraded, not Configuring.
#
# Asserts:
#   * during startup the phase is Pending/Configuring and NEVER Degraded;
#   * the set reaches Ready (and the Ready condition is True);
#   * after wedging degr-rs-1: pod Running but NotReady -> phase=Degraded,
#     Degraded condition True (2-instance set: quorum is 2, so it is also lost);
#   * after deleting the wedged pod: the set recovers to Ready.
#
# Usage:
#   test/e2e/kind-degraded.sh
#   KEEP=1 test/e2e/kind-degraded.sh
#
# Requires: kind, kubectl, go, and a running Docker daemon.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-degraded}"
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

fail() { echo "FAIL: $*"; kubectl get pods,sts,replicaset.db.tarantool.io 2>/dev/null; exit 1; }

phase() { kubectl get replicaset.db.tarantool.io degr-rs -o jsonpath='{.status.phase}' 2>/dev/null || true; }
condition() { # condition <type> -> status (True/False)
  kubectl get replicaset.db.tarantool.io degr-rs \
    -o jsonpath="{.status.conditions[?(@.type=='$1')].status}" 2>/dev/null || true
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
  grep -q 'Starting workers.*db.tarantool.io' "$OP_LOG" 2>/dev/null && break
  kill -0 "$OP_PID" 2>/dev/null || { echo "operator exited early:"; cat "$OP_LOG"; exit 1; }
  sleep 2
done

echo "==> apply the degraded-phase fixture"
kubectl apply -f test/e2e/testdata/degraded.yaml >/dev/null

echo "==> startup: phase must pass through Pending/Configuring and never Degraded"
saw_configuring=""
p=""
for _ in $(seq 1 150); do
  p="$(phase)"
  case "$p" in
    Degraded) fail "phase=Degraded during initial convergence (must be Configuring)" ;;
    Configuring) saw_configuring=1 ;;
    Ready) break ;;
  esac
  sleep 2
done
[ "$p" = "Ready" ] || fail "replica set never reached Ready (phase=$p)"
[ -n "$saw_configuring" ] || echo "    (note: startup was too fast to observe Configuring — Ready asserted)"
[ "$(condition Ready)" = "True" ] || fail "Ready condition is not True after startup"
echo "    Ready reached; Configuring observed: ${saw_configuring:-no}"

echo "==> wedge degr-rs-1's TX thread (infinite Lua loop over the admin console)"
# The eval never returns; time out the CLIENT and leave the server spinning.
kubectl exec degr-rs-1 -- sh -c \
  "echo 'while true do end' | timeout 3 tt connect /var/run/tarantool/admin.socket" >/dev/null 2>&1 || true

echo "==> the pod must stay Running but become NotReady (probe can no longer eval)"
kubectl wait --for=condition=Ready=false pod/degr-rs-1 --timeout=120s >/dev/null \
  || fail "degr-rs-1 never became NotReady after the TX wedge"
pod_phase="$(kubectl get pod degr-rs-1 -o jsonpath='{.status.phase}')"
[ "$pod_phase" = "Running" ] || fail "degr-rs-1 pod phase=$pod_phase, want Running (alive but stuck)"

echo "==> the replica set must report Degraded (not Configuring)"
p=""
for _ in $(seq 1 60); do
  p="$(phase)"
  [ "$p" = "Degraded" ] && break
  [ "$p" = "Configuring" ] && fail "phase=Configuring for a previously-Ready set (want Degraded)"
  sleep 2
done
[ "$p" = "Degraded" ] || fail "phase=$p after the TX wedge, want Degraded"
# 2-instance set: quorum is 2, so the quorum-based Degraded condition fires too.
[ "$(condition Degraded)" = "True" ] || fail "Degraded condition is not True"
echo "    phase=Degraded, Degraded condition True"

echo "==> recover: delete the wedged pod; the StatefulSet recreates it"
kubectl delete pod degr-rs-1 --wait=false >/dev/null
p=""
for _ in $(seq 1 90); do
  p="$(phase)"
  [ "$p" = "Ready" ] && break
  sleep 2
done
[ "$p" = "Ready" ] || fail "replica set did not recover to Ready (phase=$p)"
[ "$(condition Degraded)" = "False" ] || fail "Degraded condition did not clear after recovery"

echo "PASS: degraded-phase e2e succeeded (Configuring at startup, Degraded on stuck instance, Ready on recovery)"

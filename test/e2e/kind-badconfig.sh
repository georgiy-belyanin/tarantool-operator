#!/usr/bin/env bash
#
# End-to-end test for the config-first contract under a BAD configuration. The
# operator renders spec.config through to Tarantool verbatim and does not
# validate it ("Tarantool is the authority"); this suite pins down what that
# means when a user ships a typo:
#
#   1. the operator still renders and delivers the config — the Cluster reaches
#      Ready (its job is render+deliver, not validation) and the rendered Secret
#      contains the typo verbatim;
#   2. Tarantool rejects the config on startup, so the instance CrashLoops and
#      the ReplicaSet never becomes ready — the operator does NOT mask the
#      failure or report false health (the rejection reason is in the pod log);
#   3. fixing the config recovers the cluster with no manual pod intervention —
#      the operator re-renders, the Secret updates and the instance restarts
#      onto the good config and becomes ready.
#
# The typo is memtx.memroy (for memtx.memory); Tarantool reports
# '[cluster_config] memtx: Unexpected field "memroy"'.
#
# The operator runs out-of-cluster, like the other config suites.
#
# Usage:
#   test/e2e/kind-badconfig.sh
#   KEEP=1 test/e2e/kind-badconfig.sh
#
# Requires: kind, kubectl, go, and a running Docker daemon.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-badconfig}"
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

fail() { echo "FAIL: $*"; kubectl get pods,replicaset.db.tarantool.io 2>/dev/null; exit 1; }

config_has() { # config_has <substring> : succeeds if the rendered Secret contains it
  kubectl get secret bad-config -o jsonpath='{.data.config\.yaml}' 2>/dev/null | base64 -d | grep -q "$1"
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

echo "==> apply a cluster with a config typo (memtx.memroy)"
kubectl apply -f - >/dev/null <<'YAML'
apiVersion: db.tarantool.io/v2alpha1
kind: Cluster
metadata: { name: bad, namespace: default }
spec:
  config:
    memtx:
      memroy: 134217728
---
apiVersion: db.tarantool.io/v2alpha1
kind: ReplicaSet
metadata: { name: bad-rs, namespace: default }
spec:
  clusterName: bad
  replicas: 1
  podTemplate:
    spec:
      containers:
        - name: tarantool
          image: tarantool/tarantool:3
          ports: [{ name: iproto, containerPort: 3301 }]
YAML

echo "==> 1) the operator renders and delivers the bad config (no validation)"
deadline=$(( SECONDS + 120 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  [ "$(kubectl get cluster.db.tarantool.io bad -o jsonpath='{.status.phase}' 2>/dev/null)" = "Ready" ] && break
  sleep 3
done
[ "$(kubectl get cluster.db.tarantool.io bad -o jsonpath='{.status.phase}' 2>/dev/null)" = "Ready" ] \
  || fail "Cluster did not reach Ready (render must succeed even for an invalid config)"
config_has 'memroy' || fail "rendered Secret should contain the typo verbatim (the operator does not validate)"
echo "    Cluster Ready; Secret carries the typo verbatim"

echo "==> 2) Tarantool rejects it: the instance CrashLoops and never becomes ready"
ok=""
deadline=$(( SECONDS + 150 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  waiting="$(kubectl get pod bad-rs-0 -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null)"
  restarts="$(kubectl get pod bad-rs-0 -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || echo 0)"
  if [ "$waiting" = "CrashLoopBackOff" ] || { [ -n "$restarts" ] && [ "$restarts" -ge 2 ]; }; then ok=1; break; fi
  sleep 4
done
[ -n "$ok" ] || fail "instance did not CrashLoop on the invalid config"
[ "$(kubectl get statefulset bad-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" != "1" ] \
  || fail "ReplicaSet became ready on an invalid config (the failure must not be masked)"
echo "    instance CrashLoopBackOff; ReplicaSet not ready"

echo "==> 2b) the operator surfaces Tarantool's rejection (names itself)"
# Phase Error (not a misleading Configuring), Degraded=True/ConfigRejected with
# Tarantool's reason in the message, and a ConfigRejected event mentioning the
# typo. Read from pod status (terminationMessagePolicy: FallbackToLogsOnError),
# no kubectl logs needed.
ok=""
deadline=$(( SECONDS + 150 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  msg="$(kubectl get replicaset.db.tarantool.io bad-rs -o jsonpath='{.status.conditions[?(@.type=="Degraded")].message}' 2>/dev/null)"
  reason="$(kubectl get replicaset.db.tarantool.io bad-rs -o jsonpath='{.status.conditions[?(@.type=="Degraded")].reason}' 2>/dev/null)"
  case "$reason:$msg" in ConfigRejected:*memroy*) ok=1; break ;; esac
  sleep 4
done
[ -n "$ok" ] || fail "ReplicaSet did not surface the config rejection on the Degraded condition (reason=$reason msg=$msg)"
[ "$(kubectl get replicaset.db.tarantool.io bad-rs -o jsonpath='{.status.phase}' 2>/dev/null)" = "Error" ] \
  || fail "ReplicaSet phase should be Error on a config rejection"
# The event is deduped (one per reason), so assert presence, not count.
kubectl get events --field-selector reason=ConfigRejected -o jsonpath='{range .items[*]}{.message}{"\n"}{end}' 2>/dev/null \
  | grep -q 'memroy' || fail "no ConfigRejected event carrying the rejection reason"
echo "    phase=Error, Degraded=ConfigRejected (reason names the typo), ConfigRejected event emitted"

echo "==> 3) fixing the config recovers the cluster (no pod surgery)"
kubectl patch cluster.db.tarantool.io bad --type json \
  -p '[{"op":"replace","path":"/spec/config","value":{"memtx":{"memory":134217728}}}]' >/dev/null
ok=""
deadline=$(( SECONDS + 240 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  [ "$(kubectl get statefulset bad-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" = "1" ] && { ok=1; break; }
  sleep 4
done
[ -n "$ok" ] || fail "cluster did not recover after the config was fixed"
config_has 'memroy' && fail "rendered Secret still contains the typo after the fix"
config_has 'memory' || fail "rendered Secret should carry the corrected memtx.memory"
# Recovery clears the surfaced rejection: phase leaves Error and Degraded is no
# longer ConfigRejected.
[ "$(kubectl get replicaset.db.tarantool.io bad-rs -o jsonpath='{.status.phase}' 2>/dev/null)" != "Error" ] \
  || fail "ReplicaSet phase still Error after recovery"
[ "$(kubectl get replicaset.db.tarantool.io bad-rs -o jsonpath='{.status.conditions[?(@.type=="Degraded")].reason}' 2>/dev/null)" != "ConfigRejected" ] \
  || fail "Degraded still ConfigRejected after recovery"
echo "    fixed config delivered; instance reached 1/1 ready; rejection cleared"

echo "PASS: bad-config e2e succeeded (rendered verbatim, CrashLoop not masked, recovers on fix)"

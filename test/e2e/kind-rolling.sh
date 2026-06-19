#!/usr/bin/env bash
#
# End-to-end test for a ROLLING binary update under live load (tarantool 3.6 ->
# 3.7). Where kind-upgrade.sh verifies the upgrade orchestration, this suite
# verifies the SERVICE guarantees while the set rolls one instance at a time:
#
#   * no read downtime — a poller hits all instances every second over the
#     whole rollout and at every tick at least one instance must answer;
#   * no acknowledged write is lost — a writer pumps sequential ids into a
#     SYNCHRONOUS space (is_sync=true: an ack means the write reached a quorum,
#     so it must survive any single-instance restart) for the whole rollout,
#     advancing only on ack; afterwards every instance must hold exactly the
#     preloaded rows plus every acknowledged id;
#   * the rollout converges (3/3 ready on 3.7) and the operator runs the
#     schema upgrade.
#
# The operator runs IN-CLUSTER (leader step-down + schema upgrade need iproto
# access to the pods).
#
# Usage:
#   test/e2e/kind-rolling.sh
#   KEEP=1 test/e2e/kind-rolling.sh
#
# Requires: kind, kubectl, go, and a running Docker daemon.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-rolling}"
IMG="tarantool-operator:e2e"
OLD_TT="tarantool/tarantool:3.6"
NEW_TT="tarantool/tarantool:3.7"
PRELOAD=1000
PODS="roll-rs-0 roll-rs-1 roll-rs-2"

WORK="$(mktemp -d)"
ACKED="$WORK/acked"       # one acknowledged id per line, sequential
OUTAGES="$WORK/outages"   # one line per reader tick where NO instance answered
STOP="$WORK/stop"
WRITER_PID=""
READER_PID=""

cleanup() {
  set +e
  touch "$STOP"
  [ -n "$WRITER_PID" ] && kill "$WRITER_PID" 2>/dev/null
  [ -n "$READER_PID" ] && kill "$READER_PID" 2>/dev/null
  if [ -n "${KEEP:-}" ]; then
    echo "==> KEEP set: leaving kind cluster '$CLUSTER' running"
  else
    echo "==> tearing down"
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

fail() { echo "FAIL: $*"; kubectl get pods -o wide 2>/dev/null; kubectl -n tt-operator logs deploy/tt-operator --tail=40 2>/dev/null; exit 1; }

tt_eval() { # tt_eval <pod> <lua> — eval over the pod's local admin console
  kubectl exec "$1" -- sh -c "echo '$2' | timeout 5 tt connect /var/run/tarantool/admin.socket 2>/dev/null" 2>/dev/null
}

for bin in kind kubectl go docker; do
  command -v "$bin" >/dev/null || { echo "missing required tool: $bin"; exit 1; }
done

echo "==> create kind cluster '$CLUSTER'"
kind create cluster --name "$CLUSTER" >/dev/null

echo "==> build operator image and load it into kind"
docker build -t "$IMG" . >/dev/null
kind load docker-image "$IMG" --name "$CLUSTER" >/dev/null

echo "==> install CRDs and deploy the operator in-cluster"
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
kubectl apply -f test/e2e/testdata/rolling.yaml >/dev/null
deadline=$(( SECONDS + 600 ))
while [ "$SECONDS" -lt "$deadline" ]; do
  [ "$(kubectl get statefulset roll-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" = "3" ] && break
  sleep 5
done
[ "$(kubectl get statefulset roll-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = "3" ] || fail "replica set did not reach 3/3 on ${OLD_TT}"

leader() { # find the writable instance
  for p in $PODS; do
    case "$(tt_eval "$p" 'return box.info.ro')" in *false*) echo "$p"; return 0 ;; esac
  done
  return 1
}

echo "==> create a synchronous space and preload ${PRELOAD} rows"
ldr="$(leader)" || fail "no writable leader"
tt_eval "$ldr" 'box.schema.space.create("bench", {if_not_exists=true, is_sync=true}):create_index("pk", {if_not_exists=true})' >/dev/null
tt_eval "$ldr" "box.atomic(function() for i = 1, ${PRELOAD} do box.space.bench:replace({i}) end end) return box.space.bench:count()" \
  | grep -q "$PRELOAD" || fail "preload failed"
for p in $PODS; do
  deadline=$(( SECONDS + 60 ))
  until tt_eval "$p" 'return box.space.bench:count()' | grep -q "$PRELOAD"; do
    [ "$SECONDS" -lt "$deadline" ] || fail "$p did not replicate the preload"
    sleep 2
  done
done
echo "    ${PRELOAD} rows on all instances"

echo "==> start the availability reader and the ack-gated writer"
(
  # Reader: every second, the data must be readable SOMEWHERE.
  while [ ! -f "$STOP" ]; do
    ok=""
    for p in $PODS; do
      if tt_eval "$p" 'return box.space.bench:count()' | grep -qE '^- [0-9]+'; then ok=1; break; fi
    done
    [ -n "$ok" ] || date '+%H:%M:%S' >> "$OUTAGES"
    sleep 1
  done
) &
READER_PID=$!
(
  # Writer: sequential ids into the sync space; advance ONLY on ack. An ack on
  # an is_sync space means quorum replication — that id must never be lost.
  n=$(( PRELOAD + 1 ))
  while [ ! -f "$STOP" ]; do
    for p in $PODS; do
      out="$(tt_eval "$p" "if box.info.ro then return \"RO\" end box.space.bench:replace({${n}}) return \"ACK\"")"
      if echo "$out" | grep -q ACK; then
        echo "$n" >> "$ACKED"
        n=$(( n + 1 ))
        break
      fi
    done
    sleep 0.2
  done
) &
WRITER_PID=$!
sleep 5  # let both pumps prove themselves before the roll
[ -s "$ACKED" ] || fail "writer produced no acknowledged writes before the rollout"

echo "==> patch the image to ${NEW_TT} (rolling update under load)"
kubectl patch replicaset.db.tarantool.io roll-rs --type json \
  -p '[{"op":"replace","path":"/spec/podTemplate/spec/containers/0/image","value":"'"$NEW_TT"'"}]' >/dev/null

echo "==> wait for the rollout to converge (3/3 ready on the new revision)"
deadline=$(( SECONDS + 900 ))
ok=0
while [ "$SECONDS" -lt "$deadline" ]; do
  ready="$(kubectl get statefulset roll-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)"
  cur="$(kubectl get statefulset roll-rs -o jsonpath='{.status.currentRevision}' 2>/dev/null)"
  upd="$(kubectl get statefulset roll-rs -o jsonpath='{.status.updateRevision}' 2>/dev/null)"
  if [ "$ready" = "3" ] && [ -n "$cur" ] && [ "$cur" = "$upd" ]; then ok=1; break; fi
  sleep 5
done
[ "$ok" = 1 ] || fail "rollout to ${NEW_TT} did not converge"

echo "==> stop the pumps and settle"
touch "$STOP"
wait "$WRITER_PID" "$READER_PID" 2>/dev/null || true
WRITER_PID=""; READER_PID=""
sleep 5

echo "==> assert every instance runs 3.7"
for p in $PODS; do
  v="$(tt_eval "$p" 'return box.info.version' | grep -oE '3\.[0-9]+' | head -1)"
  [ "$v" = "3.7" ] || fail "$p still on $v after the rollout"
done

echo "==> assert the leader rolled LAST (leader-aware rollout)"
# On an election set with a super user the operator owns the roll (OnDelete
# strategy) and restarts the pre-roll leader only after every follower, so the
# leader's replacement pod must be the youngest of the three.
[ "$(kubectl get statefulset roll-rs -o jsonpath='{.spec.updateStrategy.type}')" = "OnDelete" ] \
  || fail "expected the operator-managed OnDelete strategy on an election set"
newest="$(kubectl get pods -l tarantool.io/replicaset=roll-rs \
  --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1:].metadata.name}')"
[ "$newest" = "$ldr" ] || fail "pre-roll leader $ldr should restart last, but $newest is the youngest pod"
echo "    leader $ldr restarted last (one leadership transition per rollout)"

echo "==> assert NO read downtime (every reader tick had an answering instance)"
[ ! -s "$OUTAGES" ] || fail "read outage ticks during the rollout: $(wc -l < "$OUTAGES") ($(head -3 "$OUTAGES" | tr '\n' ' ')...)"

echo "==> assert no acknowledged write was lost, on every instance"
last="$(tail -1 "$ACKED")"
total_acked=$(( last - PRELOAD ))
[ "$total_acked" -gt 0 ] || fail "no writes were acknowledged during the rollout"
want="$last" # rows 1..PRELOAD preloaded + PRELOAD+1..last acked, all sequential
for p in $PODS; do
  count="$(tt_eval "$p" 'return box.space.bench:count()' | grep -oE '[0-9]+' | head -1)"
  [ "$count" = "$want" ] || fail "$p holds $count rows, want $want (acked id lost or extra row)"
  tt_eval "$p" "return box.space.bench:get({${last}})[1]" | grep -q "$last" || fail "$p is missing the last acked id $last"
done
echo "    ${total_acked} writes acknowledged during the roll; all ${want} rows present on all 3 instances"

echo "==> assert the operator ran the schema upgrade"
deadline=$(( SECONDS + 240 ))
got=""
while [ "$SECONDS" -lt "$deadline" ]; do
  got="$(kubectl get replicaset.db.tarantool.io roll-rs -o jsonpath='{.status.schemaUpgradedForImage}' 2>/dev/null)"
  [ "$got" = "$NEW_TT" ] && break
  sleep 5
done
[ "$got" = "$NEW_TT" ] || fail "status.schemaUpgradedForImage=$got, want $NEW_TT"

echo "PASS: rolling-update e2e succeeded (3.6 -> 3.7 under load: no read downtime, no acknowledged write lost)"

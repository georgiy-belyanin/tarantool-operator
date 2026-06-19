#!/usr/bin/env bash
#
# Long-lasting LOAD + CHURN end-to-end test. A replicated election cluster serves
# continuous writes while the operator churns it over several cycles:
#
#   * scale the replica set UP (add instances) and DOWN (autoexpel) under load;
#   * bump memtx.memory (more RAM) — a dynamic key, hot-reloaded, no restart.
#
# Election keeps a quorum while instances roll one at a time, so the cluster keeps
# accepting writes throughout. Two pumps run over the instances' admin consoles:
#   * an ack-gated WRITER into a SYNCHRONOUS space (is_sync: an ack means the row
#     is quorum-committed, so it must survive every scale/roll), always targeting
#     the current writable leader;
#   * a READER probing read availability once per second.
#
# Hard gates:
#   * NO TRANSACTION LOST — the row count on every instance equals the number of
#     acknowledged writes, and the last acked id is readable everywhere;
#   * LIVENESS — the ack-gated writer kept advancing through the churn (the
#     cluster never stalled);
#   * reads were never stuck (~90s+); brief blips during a leader re-election are
#     measured, not failed.
#
# (Sharding scale-out/in + drain-gated removal is covered by kind-rebalance.sh,
# so this test stays unsharded and uses the stock image. Load is generated over
# iproto — k6 would need an HTTP endpoint the images don't carry.)
#
# Tunables: LOAD_CYCLES (default 1), KEEP=1 to keep the cluster.
#   NOTE: each cycle scales down (autoexpelling ordinals -3/-4) then a later cycle
#   scales back up, re-creating those pods on their RETAINED PVCs (stale expelled-
#   member data) — which currently stalls the rejoin. So >1 cycle exercises an
#   open limitation; one cycle covers the full op set (scale up, RAM, scale down)
#   under continuous load.
# Operator runs IN-CLUSTER (reload + leader observation need iproto). Requires:
# kind, kubectl, go, docker.
set -euo pipefail

cd "$(dirname "$0")/../.."

CLUSTER="${KIND_CLUSTER:-tarantool-e2e-load}"
IMG="tarantool-operator:e2e"
PRELOAD=500
CYCLES="${LOAD_CYCLES:-1}"
# every ordinal the set ever reaches (it scales between 3 and 5); the pumps probe
# all of them and skip the ones that do not currently exist.
ORDINALS="0 1 2 3 4"

WORK="$(mktemp -d)"
ACKED="$WORK/acked"     # highest acknowledged id (sequential)
OUTAGE="$WORK/outage"   # one char per failed read tick; "STUCK" if ~90s straight
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

fail() { echo "FAIL: $*"; kubectl get pods,replicaset.db.tarantool.io 2>/dev/null; kubectl -n tt-operator logs deploy/tt-operator --tail=40 2>/dev/null; exit 1; }

tt_eval() {
  kubectl exec "$1" -- sh -c "echo '$2' | timeout 6 tt connect /var/run/tarantool/admin.socket 2>/dev/null" 2>/dev/null
}

# pods that currently exist (subset of ORDINALS).
present_pods() {
  local p out=""
  for o in $ORDINALS; do
    p="load-rs-$o"
    kubectl get pod "$p" >/dev/null 2>&1 && out="$out $p"
  done
  echo "$out"
}

# the current writable (election) leader pod, or empty.
leader() {
  local p
  for p in $(present_pods); do
    case "$(tt_eval "$p" 'return box.info.ro')" in *false*) echo "$p"; return 0 ;; esac
  done
  return 1
}

wait_ready() { # wait_ready <replicas> <secs>
  local want="$1" deadline=$(( SECONDS + ${2:-300} ))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(kubectl get statefulset load-rs -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)" = "$want" ] && return 0
    sleep 4
  done
  return 1
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

echo "==> apply the cluster (3-instance election) and wait for it"
kubectl apply -f test/e2e/testdata/load.yaml >/dev/null
wait_ready 3 360 || fail "cluster did not reach 3/3"

echo "==> create the synchronous space and preload $PRELOAD rows on the leader"
ldr=""
for _ in $(seq 1 30); do ldr="$(leader)" && [ -n "$ldr" ] && break; sleep 3; done
[ -n "$ldr" ] || fail "no writable leader"
tt_eval "$ldr" 'box.schema.space.create("bench",{if_not_exists=true,is_sync=true}):create_index("pk",{if_not_exists=true})' >/dev/null
tt_eval "$ldr" "box.atomic(function() for i=1,${PRELOAD} do box.space.bench:replace({i}) end end) return box.space.bench:count()" | grep -q "$PRELOAD" || fail "preload failed"
echo "$PRELOAD" > "$ACKED"

echo "==> start the load pumps (ack-gated sync writer + 1s reader)"
(
  set +e
  n=$(( PRELOAD + 1 ))
  while [ ! -f "$STOP" ]; do
    wrote=""
    for p in $(present_pods); do
      out="$(tt_eval "$p" "if box.info.ro then return \"RO\" end box.space.bench:replace({${n}}) return \"ACK\"")"
      if echo "$out" | grep -q ACK; then echo "$n" > "$ACKED"; n=$(( n + 1 )); wrote=1; break; fi
    done
    [ -n "$wrote" ] || sleep 0.3
  done
) &
WRITER_PID=$!
(
  set +e
  streak=0
  while [ ! -f "$STOP" ]; do
    ok=""
    for p in $(present_pods); do
      tt_eval "$p" 'return box.space.bench:count()' | grep -qE '^- [0-9]+' && { ok=1; break; }
    done
    if [ -n "$ok" ]; then streak=0; else streak=$(( streak + 1 )); printf 'x' >> "$OUTAGE"; fi
    [ "$streak" -ge 90 ] && echo "STUCK" >> "$OUTAGE"
    sleep 1
  done
) &
READER_PID=$!
sleep 8
PRE_ACKED="$(cat "$ACKED")"
[ "$PRE_ACKED" -gt "$PRELOAD" ] || fail "writer made no progress before churn"
echo "    pumps running; acked up to $PRE_ACKED"

churn() {
  local cyc="$1"
  echo "==> [cycle $cyc] scale UP to 5 instances (add replicas)"
  local before after
  before="$(for o in 0 1 2; do kubectl get pod "load-rs-$o" -o jsonpath='{.metadata.uid}' 2>/dev/null; echo; done)"
  kubectl scale replicaset.db.tarantool.io load-rs --replicas=5 >/dev/null
  wait_ready 5 480 || fail "did not scale up to 5/5"
  # The fix: adding instances must NOT roll the existing ones (same pod UIDs).
  after="$(for o in 0 1 2; do kubectl get pod "load-rs-$o" -o jsonpath='{.metadata.uid}' 2>/dev/null; echo; done)"
  [ "$before" = "$after" ] || fail "scale-up rolled the existing instances (UIDs changed) — the no-roll fix regressed"
  echo "    scaled up to 5/5 WITHOUT rolling the original 3 (pod UIDs stable)"

  echo "==> [cycle $cyc] bump memtx.memory (hot reload, no restart)"
  local mem=$(( 67108864 + cyc * 67108864 )) # +64 MiB per cycle (grow-only)
  kubectl patch cluster.db.tarantool.io load --type merge \
    -p "{\"spec\":{\"config\":{\"memtx\":{\"memory\":$mem}}}}" >/dev/null
  sleep 20

  echo "==> [cycle $cyc] scale DOWN to 3 instances (autoexpel)"
  kubectl scale replicaset.db.tarantool.io load-rs --replicas=3 >/dev/null
  wait_ready 3 480 || fail "did not scale down to 3/3"
  echo "    [cycle $cyc] done; acked up to $(cat "$ACKED")"
}

for c in $(seq 1 "$CYCLES"); do churn "$c"; done

echo "==> stop the pumps and settle"
touch "$STOP"
wait "$WRITER_PID" "$READER_PID" 2>/dev/null || true
WRITER_PID=""; READER_PID=""
sleep 5

last="$(cat "$ACKED")"

echo "==> assert LIVENESS (the writer kept committing through the churn)"
[ "$last" -gt "$(( PRE_ACKED + 50 ))" ] || fail "writer barely progressed ($PRE_ACKED -> $last) — the cluster stalled"

echo "==> assert reads were never stuck (brief re-election blips are expected)"
grep -q STUCK "$OUTAGE" 2>/dev/null && fail "reads were unavailable ~90s+ — the cluster got stuck"
blips="$( [ -f "$OUTAGE" ] && wc -c < "$OUTAGE" | tr -d ' ' || echo 0 )"
echo "    read blips during churn: ${blips:-0} tick(s)"

echo "==> assert NO TRANSACTION LOST (every acked sync write present on all instances)"
for p in load-rs-0 load-rs-1 load-rs-2; do
  c="$(tt_eval "$p" 'return box.space.bench:count()' | grep -oE '[0-9]+' | head -1)"
  [ "$c" = "$last" ] || fail "$p has $c rows, want $last — a committed transaction was lost"
  tt_eval "$p" "return box.space.bench:get({${last}}) ~= nil" | grep -q true || fail "$p missing the last acked id $last"
done
echo "    $last acknowledged sync writes; all present on every instance"

echo "PASS: load+churn e2e succeeded ($CYCLES cycles under continuous load; no transaction lost, cluster never stalled)"

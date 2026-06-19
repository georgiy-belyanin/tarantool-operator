#!/usr/bin/env bash
#
# Start (and stop) a LOCAL sharded Tarantool 3 cluster for interactive testing
# of the tarantool-vshard image: kind cluster + CRDs + operator (out-of-cluster,
# kept running in the background) + the sharded demo from cluster.yaml, with
# vshard buckets bootstrapped. Everything is left RUNNING for you to poke at —
# see EXAMPLE.md next to this script for kubectl / tt command samples.
#
# Usage:
#   docker/tarantool-vshard/test.sh          # bring everything up (default)
#   docker/tarantool-vshard/test.sh status   # quick look at the cluster
#   docker/tarantool-vshard/test.sh down     # stop the operator, delete kind
#
# Requires: kind, kubectl, go, and a running Docker daemon.
set -euo pipefail

cd "$(dirname "$0")/../.."
HERE="docker/tarantool-vshard"

CLUSTER="${KIND_CLUSTER:-tarantool-local}"
IMAGE="tarantool-vshard:3"
OP_PID_FILE="/tmp/${CLUSTER}-operator.pid"
OP_LOG="/tmp/${CLUSTER}-operator.log"
KCTL=(kubectl --context "kind-${CLUSTER}")

evalon() { # evalon <pod> <lua> — evaluate Lua on a pod over its local admin socket
  "${KCTL[@]}" exec "$1" -- sh -c "echo '$2' | tt connect /var/run/tarantool/admin.socket 2>/dev/null"
}

down() {
  set +e
  if [ -f "$OP_PID_FILE" ]; then
    kill "$(cat "$OP_PID_FILE")" 2>/dev/null && echo "==> operator stopped"
    rm -f "$OP_PID_FILE"
  fi
  kind delete cluster --name "$CLUSTER" 2>/dev/null && echo "==> kind cluster '$CLUSTER' deleted"
  rm -f "$OP_LOG"
}

status() {
  "${KCTL[@]}" get clusters.db.tarantool.io,replicasets.db.tarantool.io,pods,svc,pdb 2>/dev/null
}

up() {
  for bin in kind kubectl go docker; do
    command -v "$bin" >/dev/null || { echo "missing required tool: $bin"; exit 1; }
  done

  if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    echo "==> create kind cluster '$CLUSTER'"
    kind create cluster --name "$CLUSTER" >/dev/null
  else
    echo "==> kind cluster '$CLUSTER' already exists, reusing"
  fi

  echo "==> build $IMAGE (tarantool 3 + vshard) and load it into kind"
  docker build -q -t "$IMAGE" "$HERE" >/dev/null
  kind load docker-image "$IMAGE" --name "$CLUSTER" >/dev/null

  echo "==> install CRDs"
  "${KCTL[@]}" apply -R -f config/crd/bases/ >/dev/null

  echo "==> build and start the operator (out-of-cluster, background)"
  if [ -f "$OP_PID_FILE" ] && kill -0 "$(cat "$OP_PID_FILE")" 2>/dev/null; then
    echo "    operator already running (pid $(cat "$OP_PID_FILE"))"
  else
    go build -o bin/manager .
    nohup ./bin/manager --metrics-bind-address=0 --health-probe-bind-address=0 >"$OP_LOG" 2>&1 &
    echo $! >"$OP_PID_FILE"
    disown
    for _ in $(seq 1 30); do
      grep -q 'Starting workers.*db.tarantool.io' "$OP_LOG" 2>/dev/null && break
      kill -0 "$(cat "$OP_PID_FILE")" 2>/dev/null || { echo "operator exited early:"; cat "$OP_LOG"; exit 1; }
      sleep 2
    done
    echo "    pid $(cat "$OP_PID_FILE"), log $OP_LOG"
  fi

  echo "==> apply the sharded demo cluster ($HERE/cluster.yaml)"
  "${KCTL[@]}" apply -f "$HERE/cluster.yaml" >/dev/null

  echo "==> wait for the StatefulSets to roll out (first run pulls/boots images)"
  for rs in router-a storage-a storage-b; do
    for _ in $(seq 1 60); do
      "${KCTL[@]}" get statefulset "$rs" >/dev/null 2>&1 && break
      sleep 2
    done
    "${KCTL[@]}" rollout status "statefulset/$rs" --timeout=300s >/dev/null \
      || { echo "FAIL: $rs did not become ready"; status; exit 1; }
  done

  echo "==> wait for every ReplicaSet to report Ready"
  for rs in router-a storage-a storage-b; do
    for _ in $(seq 1 60); do
      phase="$("${KCTL[@]}" get replicasets.db.tarantool.io "$rs" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
      [ "$phase" = "Ready" ] && break
      sleep 2
    done
    [ "$phase" = "Ready" ] || { echo "FAIL: $rs phase is '$phase', want Ready"; status; exit 1; }
  done

  echo "==> bootstrap vshard buckets via the router (idempotent)"
  evalon router-a-0 'vshard.router.bootstrap({if_not_bootstrapped = true}) return vshard.router.info().bucket' \
    || { echo "FAIL: vshard bootstrap"; exit 1; }

  echo
  echo "==> cluster is UP and stays up. Interact with it:"
  status
  echo
  echo "    examples:  less $HERE/EXAMPLE.md"
  echo "    operator:  tail -f $OP_LOG"
  echo "    teardown:  $HERE/test.sh down"
}

case "${1:-up}" in
  up) up ;;
  down) down ;;
  status) status ;;
  *) echo "usage: $0 [up|down|status]"; exit 2 ;;
esac

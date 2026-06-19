#!/bin/sh
# The operator joins each instance at <pod-fqdn>:<listenPort> and talks to it by
# exec'ing `tarantoolctl connect $CARTRIDGE_RUN_DIR/*.control`, so the instance
# must (a) advertise its FQDN and (b) write a .control console socket into the
# run dir. Both are derived here from the pod's identity.
set -e
FQDN="$(hostname -f)"
export TARANTOOL_INSTANCE_NAME="${TARANTOOL_INSTANCE_NAME:-$(hostname)}"
export TARANTOOL_ADVERTISE_URI="${TARANTOOL_ADVERTISE_URI:-${FQDN}:3301}"
export TARANTOOL_HTTP_PORT="${TARANTOOL_HTTP_PORT:-8081}"
export TARANTOOL_CLUSTER_COOKIE="${TARANTOOL_CLUSTER_COOKIE:-coexist-secret}"
# The operator connects via `ls $CARTRIDGE_RUN_DIR/*.control`; cartridge only
# creates the console socket when TARANTOOL_CONSOLE_SOCK is set explicitly.
export TARANTOOL_CONSOLE_SOCK="${TARANTOOL_CONSOLE_SOCK:-${CARTRIDGE_RUN_DIR}/${TARANTOOL_INSTANCE_NAME}.control}"
mkdir -p "$TARANTOOL_WORKDIR" "$TARANTOOL_RUN_DIR"
exec tarantool /app/init.lua

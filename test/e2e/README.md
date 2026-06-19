# End-to-end (kind) tests

A simple smoke test that runs the Tarantool 3 (`db.tarantool.io/v2alpha1`)
operator against a real [kind](https://kind.sigs.k8s.io/) cluster and verifies it
brings up a running, Ready Tarantool 3 instance.

## Tests

- **`kind-e2e.sh`** (`make test-e2e`) — smoke test: a single-instance cluster
  reaches Ready.
- **`kind-scaling.sh`** (`make test-e2e-scaling`) — grows a replica set from 1 to
  3 instances and shrinks it back to 1, asserting the operator scales the
  StatefulSet, re-renders the config (instances added/removed), and instances
  stay Ready.
- **`kind-config.sh`** (`make test-e2e-config`) — changes `spec.config` parameters
  (`memtx.memory`, `log.level`) on a two-instance cluster and asserts the new
  values become live (`box.cfg`) on **every** instance — render → Secret → rollout
  → live config across the whole replica set.
- **`kind-large.sh`** (`make test-e2e-large`) — a large topology: ~10 instances
  across 2 groups and 5 replica sets; asserts every instance is Ready, both groups
  and all replica sets/instances are rendered, and each replica set replicates only
  within itself (isolated from the others).
- **`kind-luaapp.sh`** (`make test-e2e-luaapp`) — runs a **user Lua application**
  inside the cluster (loaded via Tarantool 3 `app.file` from a mounted ConfigMap)
  and drives it over the admin console: the app loads, a user function computes,
  and user data-plane logic creates a space and stores/reads a row.
- **`kind-roles.sh`** (`make test-e2e-roles`) — runs a user **application role**
  (the roles mechanism, not `app.file`): a role module enabled via `spec.roles`
  and configured via `spec.rolesCfg`. Asserts the operator renders `roles`/
  `roles_cfg`, the role's `apply()` runs, it receives its config, and its
  data-plane logic stores data.
- **`kind-stack.sh`** (`make test-e2e-stack`) — the **full stack** in one cluster:
  8 instances across 2 groups / 4 replica sets (election failover) where the
  routers group runs an application **role** and the storages group runs a **Lua
  app** (`app.file`). Asserts every instance is Ready and each replica set
  replicates within itself, the config renders both groups + role + app.file, and
  data written by **both** the role and the app on the elected leader replicates to
  the follower.
- **`kind-leader.sh`** (`make test-e2e-leader`) — runs the operator **in-cluster**
  (the only e2e that does) so it can read the elected leader over iproto. Brings up
  a 3-instance election replica set, asserts `ReplicaSet.status.leader` is observed,
  then deletes the leader pod and asserts the leader is re-observed after the
  replica set re-elects.
- **`kind-reload.sh`** (`make test-e2e-reload`) — runs the operator **in-cluster**
  and verifies config **hot-reload**: with a `super` credential user, changing
  dynamic config (`memtx.memory`, `log.level`) goes live on every instance via
  `config:reload()` over iproto, with **no pod restart** (same pod UIDs, single
  StatefulSet revision).
- **`kind-nodeloss.sh`** (`make test-e2e-nodeloss`) — **dead-node remediation** on a
  multi-node kind cluster: stops a worker hosting a follower and asserts the
  operator force-deletes the stranded pod (StuckPodRemediated event), the
  StatefulSet reschedules it onto a surviving worker, and the instance rejoins
  (3/3 ready, replication intact).
- **`kind-upgrade.sh`** (`make test-e2e-upgrade`) — **binary upgrade**: a 3-instance
  election set on tarantool 3.6 is patched to 3.7; asserts the rollout converges,
  the operator then runs `box.schema.upgrade()` on the leader exactly once
  (SchemaUpgraded event, `status.schemaUpgradedForImage`), and pre-upgrade data
  survives.
- **`kind-samples.sh`** (`make test-e2e-samples`) — tests the **shipped samples**:
  render-validates every samples kustomization, then sequentially applies each
  sample runnable on the stock community image (minimal, replicated, roles,
  advanced), asserts it converges, and deletes it. The sharded and metrics samples
  are render-validated only (they need a vshard/metrics-export image; metrics also
  needs the Prometheus Operator CRDs). Runs in CI via the separate
  `samples.yml` workflow.

## Run

```sh
make test-e2e            # smoke
make test-e2e-scaling    # scale up/down
make test-e2e-config     # config propagation
make test-e2e-large      # large topology (~10 instances)
make test-e2e-luaapp     # user Lua application
make test-e2e-roles      # application role
make test-e2e-stack      # full stack: large replicated cluster + role + Lua app
make test-e2e-leader     # leader observation (operator in-cluster; failover)
make test-e2e-reload     # config hot-reload (operator in-cluster; no restart)
make test-e2e-nodeloss   # dead-node remediation (multi-node kind)
make test-e2e-upgrade    # binary upgrade 3.6->3.7 + schema upgrade
make test-e2e-samples    # shipped samples (render + runtime on community image)
# or directly:
test/e2e/kind-e2e.sh
test/e2e/kind-scaling.sh
test/e2e/kind-config.sh
test/e2e/kind-large.sh
test/e2e/kind-luaapp.sh
test/e2e/kind-roles.sh
test/e2e/kind-stack.sh
test/e2e/kind-leader.sh
test/e2e/kind-reload.sh
test/e2e/kind-nodeloss.sh
test/e2e/kind-upgrade.sh
# keep the cluster up afterwards for inspection:
KEEP=1 test/e2e/kind-e2e.sh
```

Requires `kind`, `kubectl`, `go`, and a running Docker daemon. Each script
creates and tears down its own kind cluster.

## What it does

1. Creates a kind cluster and installs the CRDs from `config/crd/bases`.
2. Builds and runs the operator **out-of-cluster** (`bin/manager` against the
   kind kubeconfig), so the test does not depend on building/pushing an image.
3. Applies `testdata/smoke.yaml` — one unsharded, single-instance cluster.
4. Asserts the operator creates the StatefulSet, the pod rolls out and passes the
   `box.info` readiness probe, the `ReplicaSet` status reaches `Ready`, and
   `box.info.status` is `running`.
5. Tears the cluster down (unless `KEEP=1`).

This exercises the full pipeline: render → config Secret → mounted file →
`TT_CONFIG`/`TT_INSTANCE_NAME` → Tarantool boot → readiness.

## Scope

Deliberately minimal and deterministic. Multi-instance replication, election
failover, leader observation, and sharding are covered by the envtest reconcile
tests (`controllers/tarantool3`) and manual runs — sharding additionally needs a
vshard-bundled image (see `AGENTS.md`).

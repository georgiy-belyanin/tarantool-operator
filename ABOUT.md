# Tarantool 3 Operator

A Kubernetes operator for **Tarantool 3** clusters built on Tarantool's native
**declarative cluster configuration** — no Tarantool Cartridge. It is the
successor to the Cartridge-based operator (`tarantool.io/v1beta1`); the new API
lives in a separate group, **`db.tarantool.io/v2alpha1`**, so both can coexist
during migration. See `AGENTS.md` for the migration plan and design history.

Status: **alpha**. The core lifecycle (deploy → configure → run → reconfigure)
works and is covered by unit, envtest, and kind end-to-end tests. Many
platform-grade features are partial or planned — see the feature matrix below.

## What it manages

Tarantool 3 is both a database and an application platform (Lua app server +
in-memory storage engine + `vshard` sharding + `roles`). The operator turns a
small set of Kubernetes resources into a running, self-configuring Tarantool 3
cluster.

Two custom resources describe the desired state:

- **`Cluster`** — Kubernetes-side cluster settings (DNS domain, a
  `credentialsSecret` naming the Secret with user passwords) plus raw Tarantool
  configuration: `config` at the global scope and `groupConfigs` per group.
  Tarantool semantics (failover, sharding, memtx, users/roles, ...) are written
  as plain Tarantool config — the operator does not re-model them as spec
  fields; Tarantool validates and interprets them. Passwords are the one
  exception: each `credentialsSecret` data key is a user name from
  `config.credentials.users`, and the operator injects the value as that user's
  password into the delivered config, so secrets never appear in CR YAML.
- **`ReplicaSet`** — one Tarantool 3 replica set (1 CR = 1 replica set = 1
  StatefulSet): which `Cluster` and `group` it belongs to, instance count, the
  pod & volume templates, plus raw config at the replica-set scope (`config` —
  e.g. `leader`, `sharding.roles`, `roles`/`roles_cfg`) and per-instance
  overrides (`instanceConfigs`). A sharded tier is expressed as several
  `ReplicaSet` objects.

## How it works

The operator is **declarative and level-triggered**: it renders the entire
Tarantool 3 configuration from the CRDs and lets each instance converge itself —
it does not drive topology imperatively (the Cartridge operator's model).

1. **Render.** `pkg/clusterconfig` maps the `Cluster` + its `ReplicaSet`s onto
   the Tarantool 3 config tree (`global → groups → replicasets → instances`):
   per-instance `iproto.listen`/`advertise`, a deterministic `bootstrap_leader`
   per replica set, and the user-supplied config sections (failover, sharding,
   credentials, app roles, ...) at their scopes — with `credentialsSecret`
   passwords injected into `credentials.users`.
2. **Deliver.** The rendered YAML is written to a **Secret** (it carries
   credentials) and mounted into every pod. The process is pointed at it with
   `TT_CONFIG` and told its identity with `TT_INSTANCE_NAME` (the pod name) via
   the downward API. Instances self-configure from this one shared file.
3. **Network.** A **headless Service** gives each pod a stable DNS name
   (`<pod>.<cluster>.<ns>.svc.<domain>`), which matches the `advertise.peer.uri`
   in the rendered config, so instances discover and replicate with each other.
4. **Run.** Each `ReplicaSet` becomes a **StatefulSet** (parallel pod
   management, PVC templates, retain-on-scale policy). Pod names equal instance
   names, so they line up with the config.
5. **Gate readiness.** An exec **readiness probe** reads `box.info.status` over
   the local admin console socket (`tt connect`), so a pod is Ready only once the
   instance reports `running` — not merely when the port opens.
6. **Reconfigure.** On any spec change the config is re-rendered and delivered;
   Tarantool itself validates it (on start and on reload). Dynamic keys are
   applied live via `config:reload()` over iproto — no restart; other changes
   roll only the replica sets whose own effective scope (global config + own
   subtree) actually changed, tracked by a per-replica-set hash. Removed
   instances are expelled gracefully via `replication.autoexpel`.
7. **Observe.** Running in-cluster, the operator reads `box.info` over iproto
   (`go-tarantool`) to report the elected leader in `ReplicaSet.status`. Status
   carries a coarse `phase` (`Degraded` once a previously-ready set regresses),
   a `ready/total` instance count, and a `Ready` condition.

Reconcilers (`controllers/tarantool3`): the `Cluster` controller owns the
Service and config Secret (rewriting the Secret only when the content hash
changes) and re-renders when any child `ReplicaSet` changes; the `ReplicaSet`
controller owns the StatefulSet and rolls it only when that replica set's scoped
config hash changes.

## Feature matrix

Legend: ✅ implemented & tested · 🟡 partial / rendered but not fully verified ·
🔭 planned · ❌ not started.

### Topology & configuration
| Feature | Status | Notes |
|---|---|---|
| Declarative cluster configuration (T3 native) | ✅ | Full tree rendered from CRDs |
| `Cluster` + `ReplicaSet` CRDs (`db.tarantool.io/v2alpha1`) | ✅ | Coexists with the Cartridge API |
| Multi-group topology (e.g. routers vs storages) | ✅ | `spec.group` per replica set |
| Config delivery (Secret-mounted file + `TT_*`) | ✅ | CE local-file path |
| Centralized config storage (etcd / TCS) | 🔭 | Enterprise; deferred (`go-storage`) |
| Passthrough config for unmodeled keys | ✅ | Operator-managed keys win on conflict |
| Config validation | ✅ | By Tarantool itself (start / config:reload); the operator passes config through |
| Rolling configuration updates | ✅ | Per-replica-set hash-triggered rollout |
| Config hot-reload (no restart) | ✅ | Dynamic keys applied via `config:reload()` over iproto (needs a `super` user); e2e-verified |
| Scoped raw config (global/group/replica set/instance) | ✅ | `spec.config`, `spec.groupConfigs`, `spec.instanceConfigs` |

### Replication & availability
| Feature | Status | Notes |
|---|---|---|
| Replication (master–replica within a replica set) | ✅ | Stable DNS peer discovery |
| Failover: `off` / `manual` | ✅ | `manual` honors `spec.leader` |
| Failover: `election` (Raft) | ✅ | Verified; use ≥3 instances for real HA |
| Failover: `supervised` | 🟡 | Rendered; external coordinator unverified |
| Deterministic initial bootstrap | ✅ | `bootstrap_leader` per replica set |
| Leader / topology status reporting | ✅ | `box.info` via `go-tarantool` |
| Self-healing (instance restart & re-converge) | ✅ | StatefulSet restart + dead-node remediation (stranded pods force-rescheduled; quorum-gated; e2e-verified) |
| Quorum protection on drains | ✅ | PodDisruptionBudget per multi-instance replica set |
| Graceful leader step-down on shutdown | ✅ | `box.ctl.demote()` preStop (election) |
| Election tuning | ✅ | `synchroQuorum`, `electionFencingMode` |

### Sharding & scaling
| Feature | Status | Notes |
|---|---|---|
| Sharding (`vshard`: roles, weight, bucket_count) | ✅ | `tarantool-vshard` image bundles vshard; per-instance `advertise.sharding` URI wired; scale-out/in e2e-verified |
| Automatic vshard bootstrap | ✅ | Once the storages are reachable, the operator runs `vshard.router.bootstrap()` through a router (idempotent; gated on a `super` user) so a fresh sharded cluster distributes its buckets with no manual step; recorded in `ReplicaSet.status.shardingBootstrapped`; e2e-verified. **Stop-gap:** bootstrapping is planned to move into Tarantool itself (platform level); the operator does it today so sharded clusters work out of the box |
| Vertical scaling (CPU/mem via pod template) | ✅ | Change rolls the replica set |
| Instance scale-up within a replica set | ✅ | Re-renders + grows StatefulSet; e2e-verified (instances join) |
| Instance scale-down within a replica set | ✅ | Pods removed + config trimmed; **graceful expel** via `replication.autoexpel` (e2e asserts no orphan in `box.info.replication`) |
| Horizontal scale-out (add shard `ReplicaSet`s) | ✅ | New storage joins; vshard rebalances buckets onto it (e2e: 50/50 → 33/34/33, no loss) |
| Safe shard removal (weight 0 → drain → delete) | ✅ | `kubectl delete` is drain-gated by a finalizer: the operator renders weight 0, waits for vshard to drain the storage to 0 buckets (verified over iproto), then releases — no data loss; e2e-verified. Needs a `super` user |

### Storage, security & operations
| Feature | Status | Notes |
|---|---|---|
| Persistent storage (PVC templates, retain policy) | ✅ | Snapshots/WAL survive rescheduling |
| Users / roles / passwords (from Secrets) | ✅ | `config.credentials.users` + `spec.credentialsSecret` (key = user name) |
| Passwords from HashiCorp Vault | 🟡 | Optional, compile-time module (`-tags vault`); annotation-driven KV v2 read, overlays the Secret. Off by default (no Vault code/dep in the stock build). See `docs/vault-credentials.md` |
| Fine-grained privileges / ACLs | 🟡 | Via passthrough config only |
| TLS/SSL for iproto | 🔭 | Enterprise feature |
| Rolling version upgrades | ✅ | One-at-a-time rollout, graceful leader hand-off, then `box.schema.upgrade()` on the leader (mixed-version-gated); 3.6→3.7 e2e |
| Backup & restore | 🟡 | Documented procedure (`box.backup`, volume snapshots) in docs/tarantool3-operations.md; Backup CRD planned |
| Schema / data migrations | ❌ | Out of scope for now |

### Application platform
| Feature | Status | Notes |
|---|---|---|
| Application roles (`roles` / `rolesCfg`) | 🟡 | Rendered; role modules must be in the image |
| Application code packaging/deploy | 🔭 | Currently the container image's responsibility |
| `crud` / router roles | 🟡 | Depends on a vshard/crud-enabled image |

### Observability & control plane
| Feature | Status | Notes |
|---|---|---|
| Status `phase` + ready/total instance count | ✅ | On both CRDs |
| Detailed status `conditions` | ✅ | `Ready` + `Available`/`Progressing`/`Degraded` |
| Scale subresource | ✅ | `kubectl scale ttrs ...`; HPA-compatible (autoscaling a quorum DB discouraged) |
| Operator metrics | ✅ | render errors, reload retries, leader-observation failures |
| Metrics / Prometheus integration | 🟡 | Sample provided (`metrics` + `roles.metrics-export` + Service/ServiceMonitor); needs a metrics-export image |
| Finalizers / ordered teardown | ✅ | Owner-reference GC is sufficient (no external state); verified — finalizers not needed |
| In-cluster image deploy | ✅ | Multi-arch image; in-cluster deploy e2e-verified by the leader-observation test |

### Testing
| Feature | Status | Notes |
|---|---|---|
| Config render golden/unit tests | ✅ | `pkg/clusterconfig` (render, hashing, scoping, validation, autoexpel) |
| Reconciler integration tests (envtest) | ✅ | `controllers/tarantool3` (incl. idempotency, secret recovery, per-replicaset rollout scoping) |
| End-to-end smoke + scaling (kind) | ✅ | `make test-e2e`, `test-e2e-scaling` (scale up/down + autoexpel) |
| End-to-end config / large / app / roles / stack (kind) | ✅ | `test-e2e-config`, `-large`, `-luaapp`, `-roles`, `-stack` |
| End-to-end leader observation (kind, in-cluster) | ✅ | `make test-e2e-leader` (election failover; `status.leader` over iproto) |

## Image requirements

The operator assumes the instance image is `tarantool/tarantool:3`-compatible:

- The entrypoint honors `TT_CONFIG` and `TT_INSTANCE_NAME` (how the operator
  selects each instance's config).
- The readiness probe runs in-pod: it needs `sh`, `grep`, and the `tt` binary, and
  a writable `/var/run/tarantool` (where the operator points the admin console
  socket via the always-on `console` config section). A minimal image lacking
  these will stay NotReady; provide a probe-compatible image or set your own
  `readinessProbe` on the pod template (the operator preserves a user-supplied one).
- Sharding (`sharding.roles`) and application roles require the `vshard` / role
  Lua modules to be present in the image.

## Known limitations

- **Sharding and app roles need an image that bundles `vshard`/role modules**;
  the community `tarantool/tarantool:3` image does not.
- **Safe *shard* removal (weight 0 → drain → delete) is not automated.** (Plain
  instance scale-down is graceful — removed instances are expelled via
  `replication.autoexpel`.)
- **vshard bootstrap is done by the operator as a stop-gap.** A fresh sharded
  cluster needs `vshard.router.bootstrap()` once to distribute buckets; the
  operator runs it automatically today (see the feature matrix). This is meant
  to be solved at the platform level — bootstrapping folded into Tarantool's own
  declarative `sharding` config — at which point the operator step becomes a
  no-op and can be dropped.
- **A replica set's own membership change still rolls that replica set.** The
  rollout hash is now scoped per replica set, so changing one replica set no
  longer rolls the others; but adding/removing an instance within a replica set
  still restarts its peers, because Tarantool reloads a file config only on
  restart and the operator does not yet hot-reload (`config:reload()`).
- **`Cluster.status.leader` is intentionally unset** — leadership is per-replica
  set in Tarantool 3; see `ReplicaSet.status.leader`.
- **Reading the leader over iproto needs in-cluster networking** to reach
  instances, so `status.leader` is populated only when the operator runs
  in-cluster (the out-of-cluster `make run` dev loop leaves it empty). Covered by
  the in-cluster `make test-e2e-leader` test.

## Quick start

```sh
kubectl apply -f config/crd/bases/                 # install CRDs
make run                                           # run the operator (out-of-cluster)
kubectl apply -f config/samples/                   # a sample cluster
make test-e2e                                      # full kind smoke test
```

See `config/samples/db.tarantool.io_v2alpha1_*.yaml` for cluster, router, and
storage examples.

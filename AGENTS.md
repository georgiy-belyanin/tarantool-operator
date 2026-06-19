# AGENTS.md — Tarantool Operator: Tarantool 2/Cartridge → Tarantool 3 Migration

## Purpose of this document

This repository currently contains a Kubernetes operator for **Tarantool 2 + Tarantool
Cartridge**. The goal is to evolve it into an operator for **Tarantool 3**, which drops
Cartridge in favor of Tarantool's native **declarative cluster configuration**. This file
records the current architecture, what changes for Tarantool 3, and a staged migration plan.

For a user-facing description of the *new* operator (how it works + a feature/status matrix),
see **[ABOUT.md](ABOUT.md)**. This file (AGENTS.md) is the internal plan and progress log.

## Progress (branch `tarantool-3-migration`)

Done and committed:
- **Stage 0** — decisions locked: CE local-file delivery, new group `db.tarantool.io/v2alpha1`,
  Go/tooling bump, official `tarantool/*` Go libs (`go-config`, `go-tarantool`, `go-storage`).
- **Stage 1** — `v2alpha1` CRD types (`Cluster`, `ReplicaSet`) + generated CRDs/deepcopy; samples
  reorganized (`config/samples/` for v2alpha1, `config/samples/cartridge/` for legacy).
- **Stage 2** — config rendering engine `pkg/clusterconfig` (CRDs → T3 config tree), with a
  canonical `Hash` for change detection; golden tests.
- **Stage 3** — reconcilers in `controllers/tarantool3` (Cluster → Service + config Secret;
  ReplicaSet → StatefulSet with mounted config, `TT_CONFIG`/`TT_INSTANCE_NAME`, hash-triggered
  rollout); `box.info` readiness probe; leader read via `go-tarantool`; envtest reconcile tests.
- **Verification** — kind e2e smoke test (`make test-e2e`) and scaling test (`make test-e2e-scaling`,
  instance scale up 1→3 and down 3→1, both verified); two e2e findings fixed (deterministic
  `bootstrap_leader`; readiness probe). See "kind e2e findings" below.

In progress / next:
- **Finding 1** — sharding & app roles need a vshard/role-bundled image (community image lacks it).
- **Dockerfile** — bump Go (≥1.22) + multi-arch so the operator can be deployed *in-cluster*
  (today it's run out-of-cluster via `make run`).
- Remaining items tracked under **Later TODOs** (JSON-schema validation, safe scale-down/shard
  removal, metrics/ServiceMonitor, finalizers, detailed status conditions) and **Open questions**.

## What exists today (Tarantool 2 / Cartridge operator)

- **Framework**: Kubebuilder v3, Go 1.20, controller-runtime v0.15.0, k8s.io/* v0.27.4.
  Module path `github.com/tarantool/tarantool-operator`. Image `tarantool/tarantool-operator`.
- **Layout**:
  - `apis/v1alpha1/` (legacy, unserved) and `apis/v1beta1/` (active CRD types).
  - `controllers/` — reconciler entrypoints (`cluster_`, `role_`, `cartridgeconfig_`).
  - `internal/` — context/controller/steps/implementation wiring (StatefulSet managers).
  - `pkg/topology/` — **the Cartridge coupling lives here** (`common.go` holds Lua scripts).
  - `pkg/reconciliation/steps/` — step implementations (`cluster/`, `role/`, `cartridge/`).
  - `pkg/election/`, `pkg/k8s/`, `pkg/events/`, `pkg/utils/`.
  - `config/` — Kustomize CRDs/RBAC/manager/prometheus/samples. `test/` — Ginkgo + mocks.
- **CRDs (v1beta1)**:
  - `Cluster` — whole cluster: `domain`, `listenPort` (3301), `failover` (FailoverConfig),
    `foreignLeader`. Status: `phase`, `bootstrapped`, `leader`.
  - `Role` — `replicasets` (count → N StatefulSets), `replicasetTemplate` (replicas,
    podTemplate, volumeClaimTemplates), `allRw`, `vshard` (vshardGroupName, clusterRoles like
    `vshard-storage`/`vshard-router`, weight). Maps to a Cartridge replicaset + roles.
  - `FailoverConfig` — `mode` (disabled/eventual/stateful/raft), `timeout`, `stateProvider`
    (etcd2/stateboard), fencing options.
  - `CartridgeConfig` — arbitrary clusterwide config YAML applied via Cartridge, with a
    blacklist (auth, topology, vshard, users_acl, schema).
- **How it works**: Pods are created via **StatefulSets** (one per replicaset, `{role}-{ord}`,
  pods `{role}-{ord}-{pod}`), a headless service gives each pod a stable FQDN advertise URI
  `{pod}.{cluster}.{ns}.svc.{domain}:{port}`. No init containers/sidecars: there is **no config
  file** — the operator drives cluster state **at runtime** by executing **embedded Lua** on a
  leader pod through `kubectl exec` → `tarantoolctl connect <socket>`.
- **Cartridge API surface used** (all in `pkg/topology/common.go`):
  - `cartridge.admin_edit_topology()` — join instances, set roles, set vshard weights.
  - `cartridge.admin_bootstrap_vshard()` — one-time vshard bootstrap.
  - `cartridge.failover_set_params()` / `failover_get_params()` — failover config.
  - `cartridge.config_get_readonly()` / `config_patch_clusterwide()` — clusterwide config.
  - `cartridge.confapplier.get_state()` — readiness gating; `box.info().uuid` — instance UUID.
- **Reconcile flow**: Cluster syncs service → waits for Roles → elects leader → bootstraps
  vshard → configures failover. Role creates/updates StatefulSets → waits Cartridge ready →
  joins instances → sets roles → waits bootstrap → sets weights. Leader persisted in
  `Cluster.status.leader` (`pkg/election`).

## What Tarantool 3 changes (research summary)

Cartridge is **deprecated and incompatible with Tarantool 3**. Tarantool 3 replaces runtime
topology administration with a **declarative YAML cluster configuration**:

- **Topology is a nested YAML tree**: `groups → replicasets → instances`. Options can be set at
  global / group / replicaset / instance scope (instance wins). A group organizes replicasets
  (e.g. separate storage vs router); a replicaset is a set of instances sharing a dataset.
- **No more `admin_edit_topology`**: membership/roles/leader are *declared*, not joined at
  runtime. The instance reads its config and converges itself.
- **Replication & failover** via `replication.failover`: `off`, `manual` (pick `leader` per
  replicaset), `election` (Raft), `supervised` (external coordinator; uses etcd for state).
  Replaces Cartridge `eventual`/`stateful`/`stateboard` and the `etcd2` state provider.
- **Sharding** via `sharding.roles: [storage]` / `[router]` per replicaset/group + a global
  `sharding` section; bucket count and weights are config keys. vshard module itself remains.
- **App roles** (`roles:` + `roles_cfg:`) are Lua modules enabled declaratively — the analog of
  Cartridge cluster roles, but configured, not joined.
- **Credentials** declared under `credentials.users`/`roles` (e.g. the `replication` role).
- **Config delivery — two options**:
  1. **Local config file** per instance (works in **open-source CE**): a YAML file with
     `{{ instance_name }}` template vars, supplied to the process; `TT_*` env vars can override.
  2. **Centralized storage in etcd** under `<prefix>/config/*`, published with
     `tt cluster publish`; the instance's local config carries only a `config.etcd` pointer
     (endpoints/prefix/credentials) and auto-reloads on change. **Enterprise Edition only.**
- **Tooling**: `tt` CLI (`tt cluster`, `tt replicaset`) and TCM (Enterprise web UI) replace the
  Cartridge WebUI and `cartridge` Lua admin.

**Design consequence**: the operator's job shifts from *imperatively driving a leader pod via
Lua* to *rendering a declarative config and delivering it to instances* (ConfigMap-mounted file
for CE; or writing to etcd for EE), then letting Tarantool 3 self-converge.

## Migration plan (staged)

### Stage 0 — Scaffolding & decisions
- **Decision (committed): target open-source CE first** using a **local config file delivered
  via ConfigMap**. This avoids the Enterprise-only etcd dependency and is the simplest path to a
  working v1. etcd-backed centralized config is explicitly **out of scope for v1** and may be
  added later as an optional delivery backend (see Stage 2).
- **Decision (committed): new API in a NEW group `db.tarantool.io`, version `v2alpha1`, Kinds
  `Cluster` + `ReplicaSet`.** Rationale discovered during Stage 1: a CRD is keyed by *group+kind*,
  so reusing `Cluster` under the existing `tarantool.io` group would make `v1beta1` (Cartridge)
  and `v2alpha1` (T3) two versions of one CRD — forcing a single storage version plus conversion.
  With no webhook, the only strategy is `None`, and CRD field **pruning** would silently strip the
  non-storage version's fields. Since the Cartridge and T3 schemas are incompatible, that breaks
  one direction or the other. Putting the T3 API in a **separate group** makes
  `clusters.db.tarantool.io` a *distinct CRD* from `clusters.tarantool.io`, so both operators
  coexist with **no conversion webhook and no pruning**, while keeping the clean short Kind names.
  `alpha1` flags the API as unstable. The old `tarantool.io/v1beta1` Cartridge CRDs are
  **deprecated but untouched** during the transition; migration is manual (recreate as
  `db.tarantool.io` resources — see Stage 5), not an in-place API conversion.
- **Decision (committed): bump Go to ≥1.22** and refresh controller-runtime / k8s.io deps; keep
  the Kubebuilder v3 layout. Add the Tarantool Go libraries below to `go.mod` (all pure-Go, so
  the `CGO_ENABLED=0` distroless build is unaffected).

Stage 0 is complete: edition = **CE local-file/ConfigMap**, API = **`tarantool.io/v2alpha1`**,
toolchain = **Go ≥1.22 + refreshed deps, Kubebuilder v3**, libraries = **go-config + go-tarantool**.

### Tarantool Go libraries to use (from the `tarantool` GitHub org)

Prefer these over hand-rolling; they track the official T3 config schema and protocol.

- **`github.com/tarantool/go-config`** (v1.3.0) — builds the T3 declarative config from
  collectors with hierarchical inheritance (global→group→replicaset→instance), JSON-Schema
  validation, configurable merge strategies, and **round-trips to YAML preserving key order and
  comments**. Key types: `Builder`, `Config` (`Get`/`Walk`/`Effective`), `MutableConfig`,
  `Collector`. **This is the engine for Stage 2** — we render config *through* it instead of
  templating YAML by hand, and get validation for free. Its `tarantool` subpackage carries
  T3-specific defaults. Collectors also cover etcd/TCS sources, useful for the deferred EE path.
- **`github.com/tarantool/go-tarantool`** (v2, stable; supports T3 over iproto) — official Go
  client. **Replaces the `tarantoolctl connect` + Lua-over-`kubectl exec` transport**: connect
  over iproto to read `box.info`/instance status for readiness/leader detection, and `Call()`
  stored functions when needed. Pure Go, fits the distroless static binary.
- **`github.com/tarantool/go-storage`** (v1.5.0) — uniform interface over centralized config
  storages (**etcd** and **Tarantool Config Storage / TCS**) with watch, transactions,
  conditional predicates, and distributed locks. **Reserved for the deferred EE delivery
  backend** (Stage 2, EE); not a v1 dependency.
- **`github.com/tarantool/go-discovery`** (v2.0.1) — *evaluated, likely not needed.* It is a
  client-side instance-discovery / connection-pool library (discovers instances from etcd,
  watches topology, balances connections). The operator already derives topology from its CRDs
  and the StatefulSet pods, so it has no need to discover the cluster. Revisit only if a control
  path ends up needing a balanced multi-instance connection pool.

### Stage 1 — New CRDs modeling the declarative tree
- `Cluster` (`db.tarantool.io/v2alpha1`) — cluster-wide config mapping to the *global scope* of
  the T3 declarative config: `domain`, `credentialsSecret` (passwords; users/roles are plain
  config), and the raw `config`/`groupConfigs` sections (config-first).
  (No etcd pointer in v1 — CE local-file.)
- `ReplicaSet` (`db.tarantool.io/v2alpha1`, replaces Cartridge `Role`) — **one CR = one Tarantool
  3 replicaset = one StatefulSet**: `clusterName`, `group`, `replicas`, `leader` (manual mode),
  `shardingRoles`, `weight`, app `roles`/`rolesCfg`, passthrough `config`, pod & volume templates.
  Design note: unlike the Cartridge `Role` (which stamped N replicasets), the T3 model is 1:1 — a
  sharded tier is expressed as several `ReplicaSet` objects (a higher-level stamping type can come
  later). This keeps the mapping to T3's `groups → replicasets → instances` honest.
- Optional `ConfigGroup` for the `groups` layer (storage vs router) — may be a field on cluster
  rather than its own CRD to keep things simple.
- Drop `CartridgeConfig`; arbitrary config becomes structured fields + a passthrough `config`
  map merged into the rendered YAML.
- Keep StatefulSet-per-replicaset + headless service + stable FQDN model — it still maps cleanly
  onto Tarantool 3 instances and their `iproto.advertise.peer` URIs.

### Stage 2 — Config rendering engine (replaces `pkg/topology`)
- New package `pkg/config` (or `pkg/clusterconfig`) that maps the CRDs to a Tarantool 3 config
  tree (groups → replicasets → instances; each instance's `iproto.listen`/`advertise`,
  `database.mode`/`leader`, `replication`, `sharding.roles`, `roles`, credentials) and renders it
  **via `go-config`** rather than templating YAML by hand. Use `go-config`'s `Builder` +
  collectors to assemble the hierarchy, lean on its JSON-Schema **validation** before publishing,
  and use its YAML round-tripping for stable, diff-friendly output. Keep the CRD→model mapping
  separate from the `go-config` render/deliver step so the delivery backend can be swapped.
- **Delivery (CE — the v1 path)**: write the rendered config to a **ConfigMap**, mount it into
  pods; the T3 entrypoint runs `tarantool --name $INSTANCE_NAME --config /etc/tarantool/config.yaml`.
  Use `{{ instance_name }}` templating and `TT_*` env so one ConfigMap serves all instances.
- **Delivery (EE — deferred, not in v1)**: a future optional backend publishes the same rendered
  config to **etcd/TCS** under `<prefix>/config/*` (the `tt cluster publish` semantics) using
  **`go-storage`** (`driver/etcd` / `driver/tcs`, with its watch/transaction/predicate support);
  pods then carry only a `config.etcd` pointer and hot-reload without restart. Tracked, not built
  in v1.
- Retire `pkg/topology/common.go` Lua scripts, the `tarantoolctl`/`podexec` transport, and the
  Cartridge readiness polling (`confapplier.get_state`). Readiness/leader detection now uses
  **`go-tarantool`** over iproto (`box.info`, instance status) plus pod health probes.

### Stage 3 — Reconcilers
- `ClusterReconciler` (`v2alpha1`): render global+topology config, reconcile the ConfigMap and
  the headless service, reconcile child ReplicaSets, surface aggregate status.
- `ReplicaSetReconciler`: reconcile the StatefulSet from templates; trigger a rollout when the
  rendered config changes (annotation/hash on pod template); honor `database.mode`/`leader`.
- **Leader election largely goes away**: in `election`/`supervised`/Raft modes Tarantool manages
  leadership. Keep a thin status reader (via **`go-tarantool`** → `box.info`) that reports the
  current leader.
- Bootstrap/vshard: no explicit `admin_bootstrap_vshard` step — sharding bootstraps from the
  declared `sharding` config. Add a guarded one-shot `vshard.router.bootstrap` only if required
  by the chosen Tarantool 3 version.

### Stage 4 — Tooling, tests, docs
- Update `Dockerfile`/`Makefile`; new sample CRs in `config/samples/`; regenerate CRDs/RBAC.
- Build out the test layers in **Local testing** below: render golden tests, real-Tarantool
  config validation, envtest reconcile tests (replace `FakeCartridgeTopology`), and a `kind` e2e.
- Add `docs/migrate-cartridge-to-tarantool3.md` covering data/topology migration for users.

### Stage 5 — Coexistence & deprecation
- Ship both API versions for a transition window; mark Cartridge CRDs deprecated. Provide a
  conversion/guidance doc (Cartridge `Role`+vshard → Tarantool 3 `ReplicaSet`+`sharding.roles`).

## Local testing

The T3 redesign makes local testing *easier* than Cartridge: behavior moves from runtime Lua
(only observable on a live cluster) to declarative config rendering (a pure function). Test in
four layers, fastest first.

1. **Render unit tests — no cluster, milliseconds.** The core of Stage 2 is CRD → config YAML via
   `go-config`. Test it as pure functions with **golden files**: feed `Cluster`+`ReplicaSet`
   fixtures, assert rendered YAML matches `pkg/config/testdata/*.yaml`, and assert `go-config`'s
   JSON-Schema validation passes/fails as expected. `go test ./pkg/config/...` (use an
   `UPDATE_GOLDEN=1` env switch to regenerate fixtures). Most correctness lives here — every
   topology/failover/sharding permutation is cheap to cover.
2. **Config validated against real Tarantool 3 — no k8s.** Boot real `tarantool/tarantool:3` from
   the rendered YAML to prove the operator emits config Tarantool accepts and converges on: a
   docker-compose of N containers sharing the config with distinct `TT_INSTANCE_NAME`, or
   `tt cluster`/`tt start`. Connect with `go-tarantool`/`tt connect` and check
   `box.info.replication`, the leader, and `vshard.storage.buckets_count()`. Catches schema drift
   the unit tests can't.
3. **Controller integration via `envtest` — `make test`.** `envtest` gives a real apiserver+etcd
   but **no kubelet (no running pods)**. Reconcilers run for real; assert the **ConfigMap,
   StatefulSet, and Service** objects are created with the expected rendered config. Replace
   `FakeCartridgeTopology` in `test/mocks` with a **fake instance-status client** implementing the
   `go-tarantool` reader interface, so reconcilers that gate on `box.info` run without live pods.
4. **Full e2e on `kind` — real pods converging.** The only layer where Tarantool runs under the
   operator: `kind create cluster` → `make docker-build IMG=tarantool-operator:dev` →
   `kind load docker-image tarantool-operator:dev` → `make install` → `make deploy IMG=...` →
   `kubectl apply -f config/samples/` (v2alpha1) → `kubectl get pods,sts,cm,svc -w`. Verify by
   port-forwarding an instance and connecting with `go-tarantool`/`tt`: `box.info.replication` all
   `follow`, expected leader, vshard buckets balanced.

**Fastest dev loop:** `make install` against `kind`, then `make run` to run the operator
out-of-cluster on the host against the kind apiserver — instant rebuilds, real pods.

## Key files to touch first

- Replace: `pkg/topology/**` (Lua/Cartridge admin), `pkg/election/**` (mostly obsolete).
- Rewrite: add `apis/v2alpha1/*_types.go` (new `Cluster`, `ReplicaSet`); `controllers/*`;
  `internal/steps/*`. Leave `apis/v1beta1/*` in place but deprecated during the transition.
- Reuse: `internal/implementation/replicasets_manger.go` (StatefulSet mgmt), `pkg/k8s/*`
  (labels/resources), `pkg/reconciliation/*` (generic step framework), `pkg/events/*`.

## Stage 3 status (reconcilers)

Implemented in `controllers/tarantool3` (fresh controller-runtime reconcilers, not
the legacy Cartridge step framework):
- **ClusterReconciler** — resolves credential Secrets, renders config via
  `pkg/clusterconfig`, reconciles the headless **Service** and the rendered-config
  **Secret** (owner-referenced), re-reconciles when any child ReplicaSet changes.
- **ReplicaSetReconciler** — renders one StatefulSet (pods = instances), mounts the
  config Secret and wires `TT_CONFIG`/`TT_INSTANCE_NAME`, stamps the pod template
  with the config hash so a config change rolls the pods; reports ready instances.
- Pure resource builders (`DesiredHeadlessService`, `DesiredStatefulSet`) with unit
  tests; both controllers registered in `main.go`; RBAC regenerated.

**Resolved earlier deferrals:** delivery vehicle = **Secret** (config carries
credentials; supersedes the "ConfigMap" wording in Stage 2); **canonical hash** =
`clusterconfig.Hash` (sha256 of sorted-JSON config tree); **Secret resolution** done
in ClusterReconciler.

## Later TODOs

- **JSON-Schema validation.** Wire the Tarantool 3 schema (via `go-config/tarantool`)
  into `clusterconfig.Render`/`validate` so invalid configs fail before delivery. Pin
  the schema to the target T3 minor version (see Open questions).
- ~~**Leader/status via go-tarantool.**~~ DONE: `ReplicaSet.Status.Leader` is populated from
  `box.info` over iproto (go-tarantool/v2) — manual mode uses `spec.leader`; election/supervised
  read `box.info.election` from a reachable instance as the config's first `super` user
  (password from `spec.credentialsSecret`).
  Failures are non-fatal (leader left empty). Needs in-cluster networking (or a port-forward) to
  reach instances — the host `go run` dev loop can't. Verified connect/auth/eval against
  tarantool 3.7. (`Cluster.Status.Leader` left unset — leadership is per-replicaset in T3.)
- **Image entrypoint contract.** Confirm `tarantool/tarantool:3` honors
  `TT_CONFIG`/`TT_INSTANCE_NAME` (verify in the kind e2e); otherwise inject command/args.
- **Reconcile tests.** envtest tests for the reconcilers (ConfigMap/STS/Service
  creation, hash-triggered rollout) and the kind e2e from Stage 4.
- **Finalizers & ordered rollout.** Owner refs cover GC today; add finalizers and
  safe leader-aware rollout coordination if needed.

## kind e2e findings (run 2026-06-02, operator out-of-cluster vs kind + tarantool/tarantool:3 = 3.7.0)

**Validated end-to-end:** render → config Secret → Secret-mounted file → `TT_CONFIG`/
`TT_INSTANCE_NAME` env → Tarantool 3 starts and applies our config (instance/replicaset
name, listen, election, metrics). Headless Service DNS + per-instance `advertise.peer.uri`
work: peers resolve and connect to each other. Operator objects (Service, config Secret,
StatefulSet) and statuses reconcile correctly; a single-instance replicaset reaches `rw`.

**Finding 1 — sharding needs vshard in the image.** Any replicaset with `sharding.roles`
makes Tarantool load the vshard module; community `tarantool/tarantool:3` does not ship it
(`E> The vshard-ee/vshard module is not available`, instance exits 1). Sharded clusters
require an image with vshard bundled. TODO: document the image requirement and/or provide a
vshard-enabled image; the samples should call this out.

**Finding 2 — fresh multi-instance election replicaset deadlock (RESOLVED).** Two brand-new
instances with `replication.failover: election` looped on `failed to authenticate` /
`Instance bootstrap hasn't finished yet (LOADING)` and never converged: the `replicator` user
exists only *after* an instance bootstraps, but with the default `auto` strategy each instance
waits to authenticate to the (still-bootstrapping) peer first. Fix: the renderer now emits a
deterministic bootstrap leader per replicaset — replicaset-scope `bootstrap_leader: <rs>-0`
(sibling of `instances`/`leader`) plus `replication.bootstrap_strategy: config`. Verified in
kind: a 2-instance election replicaset bootstraps, elects a leader, and the replica follows.
Note: `bootstrap_leader` is rejected under `replication` — it must sit at replicaset scope.

**Finding 3 — no readiness probe (RESOLVED).** Injected pods reported Ready as soon as the
process ran, overstating health. Fix: the renderer enables a local admin console
(`console.enabled: true`, `console.socket: /var/run/tarantool/admin.socket`) and the operator
injects an exec readiness probe that reads `box.info.status` over that socket
(`echo 'return box.info.status' | tt connect <socket> | grep -q running`) — no auth, and it
fails while the instance is still LOADING. A user-provided readiness probe is preserved.
Verified in kind: pods go Ready only once `box.info.status` is `running`.

**For in-cluster deploy (not just `go run`):** the `Dockerfile` still pins Go 1.19.3 and
`GOARCH=amd64`; bump Go to ≥1.22 and make it multi-arch before `make docker-build`/`deploy`.

## Open questions

- ~~CE vs EE scope~~ **Resolved: CE local-file (ConfigMap) for v1; etcd deferred.**
- Tarantool 3 minor version target (config schema evolves across 3.x); pin and document it.
- Rolling-config-change strategy for CE: ConfigMap update propagation delay + ordered restart,
  and how to coordinate leader changes safely during rollout (which `database.mode`/failover
  mode is the default, and whether a restart is always required for a given config change).
- In-place data migration story for existing Cartridge clusters (likely out of scope for the
  operator itself; document the `tt`/upgrade path instead).

## References

- Tarantool 3 configuration concepts: https://www.tarantool.io/en/doc/latest/concepts/configuration/
- Configuration reference: https://www.tarantool.io/en/doc/latest/reference/configuration/configuration_reference/
- Centralized (etcd) config storage (EE): https://www.tarantool.io/en/doc/latest/platform/configuration/configuration_etcd/
- Sharding with vshard: https://www.tarantool.io/en/doc/latest/platform/sharding/vshard_admin/
- `tt cluster` tooling: https://www.tarantool.io/en/doc/latest/reference/tooling/tt_cli/cluster/
- Tarantool 3.0 release notes: https://www.tarantool.io/en/doc/latest/release/3.0.0/
- go-config (config builder/renderer): https://github.com/tarantool/go-config
- go-tarantool (official Go iproto client): https://github.com/tarantool/go-tarantool
- go-storage (etcd/TCS centralized storage): https://github.com/tarantool/go-storage
- go-discovery (client-side discovery, evaluated): https://github.com/tarantool/go-discovery

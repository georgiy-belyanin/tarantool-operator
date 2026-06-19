# REVIEW-REAL-FINAL — Tarantool 3 Operator

Scope: everything on `tarantool-3-migration` relative to `master` (123 commits, 250 files,
+19,597/−478). Reviewed at HEAD against the actual code, not against earlier review docs.
Production code only for §4 (tests, e2e scripts and docs are out of scope there).

Production LOC at HEAD (non-test, non-generated):

| Subsystem | LOC |
|---|---|
| `controllers/tarantool3` | 1789 |
| `pkg/clusterconfig` | 944 |
| `apis/v2alpha1` | 354 |
| **Tarantool 3 total** | **3087** |
| `pkg/cartridge` + `internal/cartridge` + `controllers/cartridge` + `apis/cartridge` (legacy) | 6758 |

---

## 1. Architecture decisions review

### 1.1 Config-first CRD surface — **right call, the defining strength**

The operator mirrors none of Tarantool's option space in typed CRD fields. The CRDs carry
only what Kubernetes itself needs (replicas, pod template, PVC templates, update strategy)
plus raw passthrough config at every Tarantool scope (`Cluster.spec.config`,
`spec.groupConfigs`, `ReplicaSet.spec.config`, `spec.instanceConfigs`). The operator
computes only what it must own: names, advertise/listen URIs, bootstrap leader, password
injection.

This is the decision that keeps `apis/v2alpha1` at 354 lines and makes the operator
forward-compatible with every future Tarantool option without a CRD release. Compare:
the legacy Cartridge API needed 891 lines of types plus conversion machinery for a much
smaller feature set. Most database operators (Percona, Zalando) drown in typed config
mirrors that perpetually lag the engine; CloudNativePG's `postgresql.parameters`
passthrough is the analogous (and praised) design. Verdict: **correct, keep**.

### 1.2 Separate API group `db.tarantool.io/v2alpha1` — **correct**

A new group instead of a new version under `tarantool.io` avoided conversion webhooks and
served-version pruning entirely, and lets the legacy and new operators coexist in one
binary (`main.go` registers both controller sets). The cost — two groups in one operator —
is temporary by design. Verdict: **correct**.

### 1.3 The delivered config as the single source of truth — **strong design**

Controllers never re-derive semantics from CR fields; they parse the rendered Secret
(`Delivered`) and ask it questions (`Settings`, `IsShardStorage`, `ContainsReplicaSet`).
The pre-render reader (`userCfg`) and post-render reader (`Delivered`) share one
precedence implementation (`scopeChain`, 64 lines), so the two cannot drift. This also
gave a clean fix for the informer-warmup race (`ContainsReplicaSet` gate) — the gate reads
what was *actually delivered*, not what the cache believes. Verdict: **strong; this is
the part of the codebase to protect during future changes**.

### 1.4 Three-hash strategy (content / instance-scope / rollout) — **sound but at the complexity ceiling**

- content hash → gates Secret writes (no hot reconcile loop);
- `InstanceScopeHash` → rolls a replica set only on changes to *its* effective config;
- `RolloutHash` → additionally excludes dynamic keys + `instances` membership, so with a
  super user, dynamic changes hot-reload and scaling doesn't roll survivors.

Each exclusion is individually justified and documented, and the conservative direction of
`dynamicScopePaths` (mis-listing causes an unnecessary rollout, not a silent no-op) is the
right bias. But this is now a three-level invariant maintained by hand in two files
(`delivered.go`, `replicaset_controller.go`), and §2.1 shows one edge where the
`instances` exclusion already leaks. Verdict: **sound; do not add a fourth hash — if a new
case appears, restructure instead**.

### 1.5 Removing config schema validation (`WithoutValidation`) — **defensible, but the failure path is under-instrumented**

"Tarantool itself is the authority on what is valid" is a coherent position and eliminates
a schema-drift treadmill across Tarantool versions. The cost moved, it didn't disappear: a
config Tarantool rejects now surfaces as a pod `CrashLoopBackOff` (static path) or as a
swallowed `pcall(config:reload)` failure (dynamic path, §2.2) instead of a
`Cluster.status.phase: Error` at render time. The decision is acceptable **only if** the
runtime failure is made observable — today it isn't (no `config:info()` alert readback, no
event with the rejection reason). Verdict: **keep the decision, pay the observability debt
(§2.2, §3.2)**.

### 1.6 The "super user" capability contract — **good degradation model**

Every networked feature (leader observation, hot reload, schema upgrade, bucket drain,
drain-gated deletion) keys off one condition: a global-scope credentials user with role
`super`. With it, everything works; without it, every feature degrades to the same
conservative fallback (roll instead of reload, no leader in status, no drain gate) and the
operator says so via events. One contract instead of five feature flags. Verdict:
**good**; the nondeterministic *choice* of that user is a bug (§2.3).

### 1.7 Drain-gated shard removal via finalizer + rendered weight-0 — **correct mechanism**

The renderer emits `sharding.weight: 0` for a deleting storage set, the delete handler
pushes it live and blocks on the bucket count, and "unconfirmed count blocks" is
explicitly chosen over "unconfirmed count proceeds". Releasing the finalizer when the
Cluster/Secret/super-user is gone avoids the classic stuck-namespace failure of
finalizer-based operators. Verdict: **correct, including the failure-mode choices**.

### 1.8 Dead-node remediation gates — **appropriately paranoid**

Force-delete only when: bootstrapped, multi-instance, majority still ready, and never the
declared writable instance in off/manual modes. UID-preconditioned delete prevents
deleting a recreated successor. This matches the safety reasoning in stateful operators
that do this well (e.g. Strimzi's roller). Verdict: **correct**.

### 1.9 Build-tag Vault module — **right shape, immature transport (§2.5)**

A compile-time `PasswordSource` seam (29 lines) with a dependency-free KV v2 client keeps
the default binary free of Vault code entirely. Annotations instead of CRD fields is the
right choice for an optional module. Verdict: **right architecture, the HTTP client needs
hardening before anyone uses it in production**.

### 1.10 Readiness/preStop via unauthenticated admin socket — **pragmatic, acceptable**

Probes and `box.ctl.demote()` go through the local admin console socket, so probe
liveness never depends on credentials or iproto health. The socket is pod-local; exposure
equals `kubectl exec` privilege. Verdict: **acceptable; document it as a security note**.

### 1.11 What the architecture deliberately lacks

No webhooks, no backups, no TLS management, no aggregate cluster health. For an MVP these
are defensible cuts, but the first three are the gap between this and the operators users
will compare it to (§3). The absence of *any* admission enforcement is the one that
already bites today (§2.4).

---

## 2. Implementation problems

Ordered by severity. All verified against HEAD code.

### 2.1 Instance-scope static config changes can silently never apply (P1)

`RolloutHash` strips the whole `replicaset.instances` subtree
(`pkg/clusterconfig/delivered.go:201`) so scaling doesn't roll survivors — but
`spec.instanceConfigs` renders *under* `instances.<name>`. With a super user configured,
changing a **static** option in `instanceConfigs` therefore never changes the rollout
hash: no pod roll. The operator then calls `config:reload`, and Tarantool either applies
nothing for that key and raises a config alert ("restart required"), or the reload fails
inside `pcall` (§2.2). In the first case the instance reports the *new* config label, the
operator counts it converged, and the change is silently pending forever.

Fix options: (a) strip only `instances.*` *membership* but keep each instance's config
content in the hash (hash the set of per-instance subtrees without their names/count);
(b) read `config:info().alerts` after reload and roll the pod when Tarantool reports a
restart-required alert. Option (b) also solves §2.2.

### 2.2 `config:reload` failures are swallowed; failure mode is an unexplained 10s requeue loop (P1)

`reloadEvalScript` (`controllers/tarantool3/status.go:128`) wraps the reload in `pcall`
and discards the error; `reloadConfig` only compares hashes. A config that Tarantool
rejects at runtime produces: hash never converges → `retry=true` → requeue every 10s
forever, `reloadRetryTotal` rising, **no event, no condition, no error text anywhere**.
With schema validation removed (§1.5) this is now the *primary* failure path for bad
dynamic config and it is invisible.

Fix: capture the `pcall` error and the `config:info().alerts` list in the eval result;
emit a Warning event with the actual message; set a condition (`ConfigApplied=False`,
reason from the alert) after N failed attempts; add backoff instead of flat 10s.

### 2.3 Nondeterministic super-user selection (P2)

`firstUserWithRole` (`pkg/clusterconfig/scopechain.go:53`) ranges over a Go map. With two
`super`-role users the operator's identity flaps between reconciles — different audit
trails, and a breakage that appears/disappears if one of the users has a wrong password or
restricted listen. Fix: sort user names and pick the first deterministically (3 lines).

### 2.4 Nothing prevents mutating `spec.group` / `spec.clusterName`, and doing so bricks the ReplicaSet (P2)

The StatefulSet selector includes `tarantool.io/group`
(`controllers/tarantool3/resources.go:62`, used at `resources.go:126`), and
`reconcileStatefulSet` assigns the selector unconditionally. Selectors are immutable:
changing `spec.group` makes every subsequent update fail, and the reconcile loop errors
forever with no user-facing explanation. Changing `spec.clusterName` similarly reparents
the config subtree while the old StatefulSet keeps running. There are no webhooks and no
CEL rules. Fix: CEL immutability (`self == oldSelf`) on `spec.group` and
`spec.clusterName` — two markers, no webhook needed. (Same treatment is worth considering
for storage-affecting fields like `volumeClaimTemplates`.)

### 2.5 Vault client: `http.DefaultClient`, no CA configuration (P2, vault build only)

`credentials_vault.go:93` uses `http.DefaultClient`. Real Vault deployments are HTTPS
with a private CA; there is no way to supply one, so the module only works against plain
HTTP or public-CA Vault — i.e. not most production Vaults. Also note: anyone with
permission to edit a Cluster CR can point `tarantool.io/vault-address` at an arbitrary
in-cluster URL and have the operator GET it with a token of their choosing (mild SSRF
surface). Fix: accept a CA bundle (annotation naming a ConfigMap key) and build a
dedicated `http.Client`; consider restricting the address to an allowlist set at operator
deploy time.

### 2.6 Both controllers watch every Secret in the cluster (P2, scalability)

`ClusterReconciler` and `ReplicaSetReconciler` each register
`Watches(&corev1.Secret{}, ...)` with no predicate
(`cluster_controller.go:240`, `replicaset_controller.go:329`). Consequences: the shared
informer caches **all** Secrets cluster-wide (memory ∝ cluster size, and the operator's
RBAC already grants cluster-wide secret reads — a broad standing privilege), and every
Secret write anywhere triggers map functions that `List` CRs. On a busy multi-tenant
cluster this is real overhead. Fix: label the config Secret (already done) and use a
label-selector predicate for it; for credential Secrets, either require a marker label or
use a field-indexed lookup instead of listing; consider `cache.Options.ByObject` with a
selector so unrelated Secrets are never cached.

### 2.7 `status.leader` can lie in off-mode with a hand-picked rw instance (P3)

`observeLeader` returns instance 0 for `failover: off`
(`controllers/tarantool3/status.go:96`), but a user can set `database.mode: rw` on a
different ordinal via `instanceConfigs` (the renderer only defaults instance 0 when the
user set nothing). Status then reports the wrong leader, and remediation's "protected
writable instance" gate (`remediation.go:66`) protects the wrong pod. Fix: scan
`instanceConfigs`/delivered instances for an explicit `database.mode: rw` before
defaulting to ordinal 0.

### 2.8 Cluster phase says Ready while the data plane is down (P3, by design but user-hostile)

`Cluster.status.phase` reflects render+delivery only (`cluster_controller.go:221`
comment). A cluster with every pod down shows `Ready` at the Cluster level; the truth
lives only on each ReplicaSet. The comment justifies it, but no other database operator
exposes a top-level object whose Ready ignores member health, and users will be misled.
Fix: aggregate child ReplicaSet `Available` conditions into a Cluster-level
`MembersAvailable` condition (the watch on ReplicaSets already re-triggers the Cluster, so
this is cheap).

### 2.9 Dead return value: `reloadConfig`'s `allConverged` (P4)

Both call sites discard it (`replicaset_controller.go:211` uses only `retry`;
`shardremoval.go:83` ignores both). Remove the return value or use it to set the
`ConfigApplied` condition from §2.2.

---

## 3. Suggestions (benchmarked against other DB operators)

In priority order, with the operator that sets the precedent.

### 3.1 Backup/restore CRDs, then PITR (CloudNativePG `Backup`/`ScheduledBackup`; Percona `*-backup`)

The single biggest functional gap. Every serious DB operator ships declarative backups;
none of the peers treat it as optional. Tarantool's primitives are workable: snapshots
(`box.snapshot()`) + WAL xlogs for PITR. A minimal v1: a `Backup` CR that triggers
`box.snapshot()` on a replica and uploads `*.snap` to object storage via a sidecar/job;
`ScheduledBackup` as a cron wrapper. Restore as a bootstrap mode on `Cluster` (CNPG's
`bootstrap.recovery` is the model). The RFC already promises this section — it should be
the next implementation milestone.

### 3.2 Dry-run config validation as a warning, not a gate (CNPG's approach to `parameters`)

§1.5 removed schema validation; §2.2 shows the cost. A middle path: render, then validate
with go-config's schema *as a warning only* — emit a `ConfigSchemaWarning` event and a
condition, but still deliver. Tarantool stays the authority; the user still gets a fast
signal for typos. Zero risk of false-positive blocking, which was the reason validation
was removed.

### 3.3 Read `config:info()` alerts into status (no peer equivalent — Tarantool-specific, do it anyway)

Tarantool 3 self-reports config problems (`check_warnings`, restart-required alerts).
The operator already connects to every instance; surfacing `config:info().alerts` into a
ReplicaSet condition turns §2.1/§2.2 from silent failures into visible ones. This is the
highest-leverage 50 lines available.

### 3.4 Default pod anti-affinity (Strimzi, CNPG, Percona all do this or document it loudly)

Nothing in `DesiredStatefulSet` spreads instances across nodes. Three replicas of a
replica set can co-schedule on one node, making the PDB and quorum math meaningless. Fix:
inject a *preferred* `podAntiAffinity` on `tarantool.io/replicaset` (preserving any
user-provided affinity), or at minimum a `topologySpreadConstraint`. Soft by default so
single-node dev clusters keep working.

### 3.5 TLS (CNPG: managed certs + cert-manager integration; Strimzi: full internal CA)

No story today for encrypted iproto (client or replication) or for the operator's own
connections. Tarantool EE supports SSL transport; for CE the answer may be "document the
limitation", but the operator should at least pass through cert Secret mounts cleanly and
support TLS in its go-tarantool dialer for EE clusters.

### 3.6 Leader-aware rollouts (CNPG switchover-before-restart; MongoDB stepDown)

The election-mode preStop `box.ctl.demote()` is good. Two refinements: (a) in election
mode, restart the *current leader last* by checking observed leader vs update ordinal
(today ordinal order is leader-last only by accident of `<rs>-0` defaults); (b) in
supervised mode, integrate with the failover coordinator rather than relying on SIGTERM
drain.

### 3.7 Operational ergonomics, in rough order of value

- **Helm chart / OLM bundle** (every peer): today install is `kubectl apply -R -f config/`.
- **Namespace-scoped mode** (`WATCH_NAMESPACE`, Zalando/Percona): pairs with fixing §2.6;
  required by many enterprise platform teams.
- **Downgrade guard**: detect an image *downgrade* and refuse/warn before pods roll —
  `box.schema.upgrade()` is one-way without `box.schema.downgrade()`; peers gate this.
- **Event/condition on every degraded fallback**: `LeaderObservationDisabled` exists;
  reload-disabled and drain-disabled (no super user) deserve the same.
- **kubectl plugin** (CNPG `kubectl cnpg status`): nice-to-have; the `Delivered`
  introspection layer makes a `status`/`promote` plugin cheap to build later.

---

## 4. Code quality (production code only)

### 4.1 Overall

The Tarantool 3 code is in good shape: 3087 lines for the whole feature set is lean (the
legacy operator spends 6758 lines doing less). Strengths worth calling out because they
should be preserved under future change: one iproto seam (`iproto.go`, 87 lines — every
network call goes through `forEachInstance`/`evalInstanceString`), one precedence reader
(`scopeChain`), parse-once `Delivered`, single status writer (`observed`/`applyStatus`),
hash-gated writes, and consistently explanatory comments that state *why* (the
`ContainsReplicaSet` warmup comment, the `dynamicScopePaths` conservatism note).

### 4.2 The elephant: 69% of production Go is the deprecated operator

`pkg/cartridge` (4754) + `internal/cartridge` (654) + `controllers/cartridge` (459) +
`apis/cartridge` (891) = 6758 LOC maintained for the legacy product. Removal was proposed
(CLEANUP-REFACTOR R1/R2) and **declined by the maintainer** — that decision stands, but it
should be recorded here as the dominant code-size fact: every repo-wide change (lint
migration, dependency bumps, Go version) pays a 2× tax. When the legacy operator is
eventually dropped, this codebase halves in one commit. No action now; revisit at the
first Tarantool-3-only release.

### 4.3 Concrete reductions in the new code (small, safe)

1. **Drop `allConverged`** from `reloadConfig` (§2.9) — one return value, two call sites.
2. **PDB removal path**: `reconcilePodDisruptionBudget` does Get-then-Delete
   (`replicaset_controller.go:266`); a direct `Delete` + `IgnoreNotFound` on a stub object
   removes 5 lines and one API read.
3. **`portOf`** (`delivered.go:238`) hand-parses host:port backwards; `net.SplitHostPort`
   + `strconv.Atoi` is shorter and handles IPv6 brackets, which the current code does not
   (an IPv6 advertise URI today silently falls back to 3301).
4. **Three near-identical watch map functions** (`mapReplicaSetToCluster`,
   `mapSecretToClusters`, `mapConfigSecretToReplicaSets`) share the list-filter-enqueue
   shape; a small generic helper would save ~25 lines. Marginal — only do it if a fourth
   appears.
5. **`main.go:20` `ManagerPort 9443`**: webhook port wired with no webhooks registered;
   the `Port` option is deprecated in newer controller-runtime anyway. Delete with the
   next controller-runtime bump.

### 4.4 Non-reductions (reviewed and fine as-is)

- `render.go` at 507 lines is the largest new file but is a single linear pipeline
  (`buildTree → buildGroups → buildReplicaSet → buildInstance`) with no shared mutable
  state; splitting it would add indirection, not clarity.
- The `evalInstanceString` package-var test seam is unidiomatic but confined to one
  variable in one file; a full interface would be more code for no behavior.
- `upsertByName` generics in `resources.go` — exactly the right amount of cleverness.
- The Lua eval scripts as Go string consts (5 sites) are fine at this count; extract to
  embedded files only if they grow logic.
- `deepCopyJSON`/`deletePath`/`lookup` micro-helpers: small, tested via their callers,
  not worth a dependency.

### 4.5 Consistency nits

- `DefaultIprotoPort` logic exists in both `Settings()` and `ClusterPort()` with subtly
  different fallback paths (parse error → default vs unset → default); harmless today,
  but a comment cross-referencing them would prevent drift.
- `condition()` reason strings mix phase names (`"Ready"`, `"Pending"`) and invented
  reasons (`"QuorumLost"`); pick the CamelCase-reason convention everywhere — `kubectl
  describe` output is user API.

---

## Verdict

The architecture is right: config-first CRDs, delivered-config introspection, and the
single super-user capability contract form a coherent, minimal core that will age well.
The implementation has no structural problems — the issues found (§2) are localized and
each fixable in under a day, with §2.1/§2.2 (silent non-application of config) the only
pair I would block a production release on, because they undermine the operator's core
promise that *the delivered config is the truth*. The feature gap to peer operators is
known and honestly documented; backups (§3.1) and config-alert surfacing (§3.3) are where
the next effort buys the most.

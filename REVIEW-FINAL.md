# REVIEW-FINAL — Tarantool 3 operator: consolidated, prioritized backlog

Consolidates **REVIEW-3** (product-feature-list review) and **REVIEW-4**
(operator-craft + Tarantool-3-internals review) into one de-duplicated, prioritized
backlog for the Tarantool 3 operator (`db.tarantool.io/v2alpha1`, all of
`master..HEAD`). Every finding from both is folded in and given a stable **F-ID**;
the **traceability table (§6)** maps every original `REVIEW-3`/`REVIEW-4` item to its
F-ID so nothing is dropped. REVIEW-3 and REVIEW-4 have been folded into this document
and removed; this is now the single review of record (their full text remains in git
history if the per-finding detail is ever needed).

This is a **forward-looking architecture/feature backlog**. It does not re-litigate
the code-correctness layer already tracked and largely fixed in `REVIEW.md`
(RP1–RP6, RS1–RS6) and `PROBLEMS.md` (P1–P12) — both since removed (all items
closed; see git history); those are referenced where relevant.

Tags: **[K8S]** k8s operator best practice · **[T3]** Tarantool 3 internals ·
**[FEAT]** required product feature. Severity: **P0** correctness / availability /
data-risk or on-prem blocker · **P1** significant production gap · **P2**
quality/robustness. Type: **Defect** (wrong/buggy today) · **Gap** (missing
capability).

> **Grounding facts:**
> - **`bootstrap_strategy: native` exists since Tarantool 3.4** (Jira TNTP-366) —
>   *correcting* the earlier REVIEW-4 research, which couldn't find it in the public
>   docs. It runs the `supervised` strategy under the hood but auto-selects the
>   bootstrap leader from the cluster config: for `failover: election` it picks the
>   **lowest-named instance** (excluding anonymous replicas) — which for the
>   operator's `<rs>-0…N` naming is exactly **`<rs>-0`**, the same instance the
>   operator hardcodes today via `bootstrap_strategy: config` + `bootstrap_leader:
>   <rs>-0`; for `off`/`manual` it calls `box.ctl.make_bootstrap_leader()` on a
>   configured RW instance (the first lexicographically in multi-master, and the
>   last instance to go RW becomes the single bootstrap leader). So `native` is a
>   candidate simplification — see **F32**. (An earlier empirical attempt to switch,
>   REVIEW.md RS1, saw a bootstrap deadlock; that should be re-investigated against
>   ≥3.4 since it contradicts the documented behavior.)
> - **A mounted-file config does not hot-reload on file change** — only the etcd
>   source auto-reloads; `config:reload()` must be called explicitly (SIGHUP shuts
>   the instance down). This underpins F2.
> - **CE/EE gating:** `election` failover, vshard, `autoexpel`, `metrics` /
>   `roles.metrics-export`, `config:reload()`, anonymous replicas are **Community**.
>   **Supervised failover** (+ its etcd stateboard), **iproto TLS**, and
>   **`pap-sha256`** are **Enterprise-only** — the two EE gates constrain F-items and
>   the satisfiability matrix (§5).

---

## 0. Implementation status (this backlog is being worked)

Done — implemented, tested (unit/envtest, kind e2e where runtime behavior is
involved), committed:
- **F1** ✅ fresh-bootstrap split-brain (autoexpel gated on sticky
  `Status.Bootstrapped` + ReplicaSet waits for its subtree in the rendered config);
  full-stack e2e passes.
- **F2** ✅ operator-driven config hot-reload: dynamic keys (log.level,
  memtx.memory, replication.timeout, credentials, sharding.weight) apply via
  `config:reload()` over iproto with **no pod restart** (RolloutHash + the
  `operator_config_hash` config label; needs a `super` user, else falls back to
  roll). New in-cluster e2e `make test-e2e-reload`.
- **F5** ✅ resolved by coverage (see the item) — RollingUpdate is already
  one-at-a-time/readiness-gated; F6+F7 close the rest; operator-driven leader-last
  OnDelete judged not worth the risk.
- **F6** ✅ PDB per multi-instance replica set (maxUnavailable=1; removed at 1
  instance). **F7** ✅ graceful `box.ctl.demote()` preStop (election only).
- **F13** ✅ `/scale` subresource + `status.replicas`/`status.selector`.
- **F19** ✅ CEL: `bucketCount` immutable, `spec.leader` validated.
- **F21** ✅ (partial) single tree build per reconcile (`RenderAndHash`).
- **F22** ✅ production logging default. **F30** ✅ operator `replicas: 2`.

Also done (second batch):
- **F2** ✅ operator-driven config hot-reload (`config:reload()` over iproto,
  `RolloutHash` excludes dynamic keys, `operator_config_hash` label verification);
  in-cluster e2e `make test-e2e-reload` proves no-restart changes.
- **F3** ✅ dead-node remediation (the 1C todo): stranded pods (unreachable node /
  stuck Terminating) force-deleted after a 60s grace, gated on
  bootstrapped+majority and never the declared writable instance in off/manual;
  multi-node e2e `make test-e2e-nodeloss` proves stop-worker → reschedule →
  rejoin. **F7** ✅ graceful `box.ctl.demote()` preStop (election).
- **F20** ✅ Available/Progressing/Degraded conditions. **F29** ✅ operator
  metrics (render/reload/leader-observation). **F31** ✅ MemtxShrinkRequiresRestart
  warning (a memtx decrease can't hot-reload).

Third batch:
- **F9** ✅ binary-upgrade orchestration: post-rollout `box.schema.upgrade()` on
  the leader, gated on homogeneous `box.info.version`; tracked in
  `status.schemaUpgradedForImage`; real 3.6→3.7 e2e `make test-e2e-upgrade`
  (rollout + schema + data survival) passing.
- **F28** ✅ `spec.replication.synchroQuorum` + `electionFencingMode` rendered
  into the global replication scope (enum/number-or-expression validated).
- **F8 / F24 / F25** 📘 documented in `docs/tarantool3-operations.md` (hot-backup
  procedure via `box.backup`, NetworkPolicy example + CE no-TLS caveat,
  durability/PVC/expansion model). A `Backup` CRD remains future work — the
  storage backend choice (S3/PVC/volume-snapshots) is deliberately not baked in.

Deferred with reasons: **F10/F14** planned by team (universal probes;
vshard-in-image), **F4** opinionated (RPM delivery), **F11** needs a
non-root-image decision (stock image runs as root), **F12** needs the
standalone-vs-combined packaging decision, **F15** (external replicas) needs the
T3 mechanics pinned (anonymous-replica vs peer-list design), **F16/F17**
integration features (Prometheus-Operator/Vault dependencies — design choices),
**F23** (deps bump) is a large mechanical migration best done as a dedicated
change, **F26** becomes relevant at the beta API promotion, **F27** builds on the
verified hot-reload path (extend the reload eval with `config:info()` alerts),
**F32** (`native` bootstrap) needs a dedicated experiment — the earlier deadlock
evidence is tainted by the since-fixed autoexpel bug, but switching the bootstrap
default requires the full e2e suite re-run to justify.

## 1. What's solid (don't regress these)

A well-built declarative **renderer + bootstrapper**: CRDs → schema-validated T3
config tree (RP4) → Secret-mounted file (`TT_CONFIG`/`TT_INSTANCE_NAME`) → headless
Service + per-replicaset StatefulSets. Idempotent steady-state reconcile (RP1);
namespace-confined credential reads (RP2); per-replicaset rollout hash (P3);
`replication.autoexpel` keys correct per the 3.3 schema (P4); `box.info` leader
observation (P10); Ready conditions + `observedGeneration` (P8); events on degraded
leader observation (RS2); owner-ref GC; distroless-nonroot operator image;
kube-rbac-proxy-protected metrics; leader election enabled. Correct core T3
modelling (`bootstrap_leader`+`config`; `database.mode: rw`/`leader` to avoid the
all-read-only bootstrap failure). Cluster creation, intra-set replication, all four
failover modes, and rolling config updates are e2e-covered.

The gaps below are concentrated in **day-2 operations** and **platform
integrations** — the hard part of a stateful-DB operator.

### 1a. Already in progress / planned by the team (close these against the work when it lands)

- **Universal readiness/liveness check for Tarantool** — in progress. When it lands,
  wire the operator's pod probes to it and close **F10** against it (it should
  subsume the ad-hoc `box.info` checks).
- **vshard bundled into the image** — planned as separate later work. Closes the
  vshard half of **F4(a)** (sharding on the stock image); keep the "emit a clear
  event on an absent module" guard.
- **`bootstrap_strategy: native`** is available since **Tarantool 3.4** (TNTP-366) —
  this *corrects* an earlier finding. It makes **F32** a real option (drop the
  hardcoded `bootstrap_leader`); see the grounding note above and F32 for the exact
  semantics and the RS1 re-investigation caveat.

---

## 2. P0 — correctness / availability / on-prem blockers

### F1 · Defect · [T3][K8S] — `replication.autoexpel` is enabled from first bootstrap → split-brain on fresh bring-up
`render.go` emits `autoexpel {enabled: true, by: prefix, prefix: <rs>}` on **every**
replica set unconditionally. [T3] autoexpel acts on prefix-matching instances and,
during initial concurrent bootstrap before membership settles, expels peers.
**Reproduced in this repo's full-stack e2e:** every fresh 3-node `election` set
split-brained (`-0` alone, `-1`/`-2` a separate replicaset, `REPLICASET_UUID_MISMATCH`);
removing the `autoexpel` block and recreating the pods made the set form cleanly.
**Fix:** don't enable autoexpel at t0 — gate it on the replica set being formed
(enable once status shows all instances joined, or only when membership is
*shrinking*), so it serves scale-down without sabotaging bootstrap. *Highest-impact
bug found.* (R4-I12)

### F2 · Defect+Gap · [T3][K8S] — config change always restarts pods, and the "mounted-file auto-reload" assumption is false
The operator delivers config as a Secret-mounted file and triggers a **StatefulSet
rollout** on any change. [T3]: a local-file config does **not** auto-reload on file
change (only the etcd source does); `config:reload()` must be called explicitly
(SIGHUP shuts down); and a projected-Secret update only lands after the kubelet sync
period. Meanwhile **most keys are dynamic** (`box.cfg` `Dynamic: yes`: `replication`,
`listen`, `log`, credentials, `memtx.memory` grow), so the restart is usually
unnecessary; only `Dynamic: no` keys (`wal.dir`, `memtx.dir`, `vinyl.dir`,
`wal.mode`, `wal.max_size`) truly need one.
**Fix:** after the Secret updates, call `config:reload()` over the admin console for
dynamic-only diffs and restart only when a `Dynamic: no` key changed; or adopt the
etcd backend (`PLAN-CONFIG-STORAGE.md`, M1 ships independently) for native
watch+reload. Surface `config:info().status`/`alerts` into status to confirm a
reload applied (see F27). (R4-I11, R3-I6, R3-S6)

### F3 · Gap · [K8S][T3] — no dead-minion / stuck-pod remediation (the 1C todo)
[K8S] a StatefulSet will not replace a pod on a lost node (at-most-one identity), and
an RWO PVC bound to a dead node can't reattach, so a dead "minion" leaves a
permanently-down replica; force-deleting without fencing risks two live pods with
one identity → split-brain. [T3] a replica *can* rebuild from the leader via rejoin,
and fencing exists (`election_fencing_mode`). The operator does nothing on node loss.
**Fix:** a remediation controller that, after a node is confirmed gone past a
configurable grace period, **fences** the member (autoexpel/expel from `_cluster`)
then force-deletes the pod so the StatefulSet recreates it and it rejoins — gated on
quorum, `replicas ≥ 2`, and never the last writable instance; handle the RWO-volume
detach (or recommend a forced-detach CSI / RWX / local-with-rebuild). This is the
explicit "реанимация упавших реплик с вышедшим из строя миньоном" todo. (R4-I13,
R3-I1, R3-S7) — pairs with F6 (PDB).

### F4 · Gap · [FEAT][K8S] — no application/module delivery model (apps, vshard, roles, metrics need a non-stock image)
The single biggest on-prem gap, in two coupled parts:
- **(a) Module/image dependency.** `vshard`, `roles.metrics-export`, and most app
  `roles` are not in the stock community `tarantool/tarantool:3` image (vshard
  verified absent; metrics-export is built into the core binary so **likely
  present** — re-verify, see F16). The operator renders valid config but instances
  `CrashLoopBackOff` on the stock image, with no clear signal. So sharding (FEAT 5),
  the Prometheus exporter (FEAT 8), and most Lua apps (FEAT 2) don't work out of the
  box. (R3-I2) · **Planned (team):** bundling **vshard into the image** is tracked as
  separate later work — when it lands, mark this sub-part done and keep only the
  "emit a clear event on an absent module" guard.
- **(b) RPM/Lua app delivery.** There is no model for delivering user apps as RPMs
  (CentOS 7/8) — apps arrive only via a custom image, a ConfigMap-mounted Lua file +
  `LUA_PATH`, or `app.file` passthrough. Multi-app *topology* works (per-replicaset
  `roles`/`rolesCfg`), but RPM packaging/delivery is absent. This is pure k8s-side
  work — **no T3 gate**. (R3-S1)
**Fix:** ship (or provide a build recipe for) a bundled image carrying vshard +
metrics-export + the role SDK; **emit a clear status/event** when `shardingRoles`/
`roles` reference an absent module; and add an **`Application`** abstraction (CRD or
ReplicaSet field) — artifact ref (RPM/rock URL or registry), version, target
group/replica sets — delivered via an init-container `dnf install` into a shared
`LUA_PATH` volume (or a per-cluster image build), auto-wiring `roles`/`rolesCfg`.
Multiple `Application`s ⇒ multi-app. (Unblocks FEAT 2, and the module half of 5/8.)

---

## 3. P1 — significant production gaps (day-2 safety, integrations, security)

### F5 · Gap · [K8S][T3] — rolling updates are not leader-aware or quorum-gated · ✅ RESOLVED (by coverage)
**Resolution (decision):** investigation showed the premise was partly inaccurate —
StatefulSet `RollingUpdate` already restarts **one pod at a time, readiness-gated**
(`ParallelPodManagement` affects only scaling, not updates), reverse-ordinal, which
is leader-**last** for `off`/`manual` (leader = ordinal 0). With **F6** (PDB) and
**F7** (graceful `box.ctl.demote()` preStop making the election leader's mid-rollout
restart a near-instant handoff), the remaining delta — forcing leader-last for
*election*'s dynamic leader via operator-driven OnDelete — was judged not worth
replacing the battle-tested RollingUpdate. Accepted as covered. Original below.

StatefulSets use `ParallelPodManagement` + default `RollingUpdate`, so a config/
image/resource change can restart the leader and multiple instances at once →
avoidable failover, write-availability gap, transient quorum loss. [K8S] mature DB
operators roll **one at a time, replicas first, leader last**, gating each step on
the previous member rejoining and quorum staying healthy; [T3] step the leader down
with `box.ctl.demote()` first and check `box.info.replication[].upstream.status ==
follow` before proceeding. **Fix:** `OnDelete` (or `RollingUpdate` + `partition`) +
operator-driven ordered, leader-last restart gated on `box.info`. This is the shared
mechanism behind config change (FEAT 3), scaling (FEAT 4), and upgrades (F9).
(R4-I15, R3-I3, R3-S5)

### F6 · Gap · [K8S][T3] — no PodDisruptionBudget
[T3] election quorum is ⌊N/2⌋+1; [K8S] a node drain can evict a majority → read-only/
unavailable. The operator emits no PDB. **Fix:** emit a PDB per replica set
(`maxUnavailable: 1`, or `minAvailable` = quorum for election sets). Pairs with F3 to
make node operations safe. (R4-I14, R3-I4)

### F7 · Gap · [K8S][T3] — no graceful shutdown / leader step-down
Instance pods get no `preStop` hook and the default `terminationGracePeriodSeconds`.
[T3] SIGTERM triggers `box.ctl.on_shutdown` + a graceful iproto drain, and
`box.ctl.demote()` hands off leadership immediately instead of waiting for the
fencing/election timeout. **Fix:** inject a `preStop` (admin-console
`box.ctl.demote()` on the leader) and raise `terminationGracePeriodSeconds` ≥ drain
budget; pairs with F5. (R4-I16)

### F8 · Gap · [K8S][T3] — no backup/restore
[T3] `box.backup.start/stop`, `box.snapshot()`, `snapshot.*`/`checkpoint` keys exist;
[K8S] mature DB operators ship `Backup`/`ScheduledBackup` CRDs with retention + PITR.
The operator has none → untested restores, unknown RPO/RTO. **Fix:** at minimum
document a backup procedure; ideally a `Backup`/`ScheduledBackup` CRD driving
`box.backup`. (R4-I17)

### F9 · Gap · [K8S][T3] — no version-upgrade orchestration
Bumping the image rolls pods with no ordering and no schema step. [T3] a correct
binary upgrade is replicas-first/leader-last, then **`box.schema.upgrade()`** on the
leader once all binaries are new, with the **`compat`** module pinning changed
defaults; adjacent minor versions can replicate during the window. **Fix:**
orchestrate the upgrade (ties to F5), run `box.schema.upgrade()` post-rollout, expose
`compat` via passthrough, and gate disallowed version jumps. (R4-I18)

### F10 · Defect+Gap · [T3][K8S] — probes are shallow (no liveness, no startup; readiness too coarse)
Only a readiness probe (`box.info.status == running`); no liveness, no startup probe.
[T3]: a replica's readiness should also require `upstream.status == follow` with
bounded `idle`/`lag` (and `config:info().status == ready` after a reload); a
**startup** probe should cover long WAL replay; a **liveness** probe must check
*only* process liveness — it must **not** gate on `box.info.ro`, or it will kill
healthy read-only replicas and leaders mid-election. **Fix:** add a process-only
liveness probe + a startup probe; enrich readiness for replicas (the
`roles.metrics-export` `health` endpoint is a convenient target). (R4-I20, R3-I7)
**In progress (team):** a **universal readiness/liveness check for Tarantool** is
being built; when it lands, wire the operator's probes to it (it should subsume the
ad-hoc `box.info.status`/`upstream` checks above) and close this item against it.

### F11 · Gap · [K8S] — managed pods get no operator-applied securityContext
Only the operator's own pod is hardened; rendered StatefulSets carry only what the
user puts in `podTemplate`. **Fix:** apply Restricted-profile defaults to instance
pods (`runAsNonRoot`, `seccompProfile: RuntimeDefault`, `allowPrivilegeEscalation:
false`, drop `ALL` caps; `readOnlyRootFilesystem` with writable data via the PVC),
overridable by the user. Lets clusters run in Pod-Security-Restricted namespaces.
(R4-I21)

### F12 · Defect · [K8S] — operator RBAC is over-broad for the Tarantool 3 controllers
A single shared `ClusterRole` serves both the Cartridge and T3 controllers and grants
cluster-wide `secrets` (incl. create/delete), **`pods/exec`**, `persistentvolumes`,
`endpoints` — several are Cartridge-only (`pods/exec`), and cluster-wide secret
write/delete is the highest blast radius. **Fix:** split T3 RBAC — drop `pods/exec`/
`persistentvolumes`/`endpoints`, scope `secrets` to actual use, offer a namespaced-
`Role` deployment mode. (R4-I5)

### F13 · Gap · [K8S] — no `/scale` subresource; replica count not machine-readable
`ReplicaSet` exposes no scale subresource, and `status.readyInstances` is a string
(`"2/3"`) with no integer `status.replicas`/`status.selector`. So `kubectl scale
ttrs` and HPA can't target it. **Fix:** add `+kubebuilder:subresource:scale`
(specpath `.spec.replicas`, statuspath `.status.replicas`, selectorpath
`.status.selector`), populate both; document HPA as quorum-discouraged. (R4-I1, R3-S8)

### F14 · Gap · [T3][FEAT] — sharding/vshard lifecycle not orchestrated (FEAT 5 day-2)
Adding a storage `ReplicaSet` renders into `sharding`, but vshard **bucket bootstrap
and rebalancing** are unmanaged: no guarded one-shot `vshard.router.bootstrap()`, and
shard *removal* doesn't drain buckets first → data-loss risk. [T3] `bucket_count` is
effectively immutable post-bootstrap. **Fix:** a sharding lifecycle step — guarded
one-shot bootstrap (docs don't confirm declarative auto-bootstrap; treat as manual);
on removal set `sharding.weight: 0` and wait for the storage's bucket count to reach
0 before expelling; make `bucketCount` immutable (see F19). (R4-S4, R3-I5)

### F15 · Gap · [T3][FEAT] — no external replicas / federation (FEAT 6)
Topology is closed to the operator's own pods (advertise URIs all `<pod>.<svc>`); no
way to add an external instance as a replica (the Cartridge `foreignLeader` analog).
**Fix:** model external read replicas via [T3] `replication.anon: true` (anonymous,
read-only, not in `_cluster`) and/or let a ReplicaSet declare external peer URIs the
renderer adds to the instance/peer list without creating pods. CE-feasible. (R4-S3,
R3-S4)

### F16 · Gap · [K8S][T3][FEAT] — observability not operator-managed (FEAT 8)
Only a hand-wired *sample*. **Fix:** when metrics are enabled, the operator emits a
metrics `Service` + `ServiceMonitor` (Prometheus Operator) selecting instance pods, a
`PrometheusRule` with baseline alerts (no leader / quorum lost, replication lag,
`arena_used_ratio` near 100%, bucket imbalance, backup failure), and a Grafana
dashboard; auto-wire `roles.metrics-export` from a `spec.observability` toggle.
**Note/correction:** the `metrics` module is built into the core CE binary, so
`roles.metrics-export` likely works on the stock image (unlike vshard) — the metrics
sample's claim that it "is NOT in the community image (same as vshard)" should be
re-verified and probably corrected. (R4-S5, R3-S3, and the metrics half of R3-I2)

### F17 · Gap · [FEAT][K8S] — no Vault secret delivery (FEAT 9)
Credentials come only from k8s Secrets (`SecretKeyReference`). **Fix:** (quick win,
works today — just document + sample) the user can add HashiCorp **Vault Agent
Injector** annotations to `spec.podTemplate.metadata.annotations` (the operator
passes pod annotations through); (first-class) support **Secrets Store CSI** /
**external-secrets**, or a `VaultSecretRef`. Prefer delegating to CSI/external-secrets
over embedding a Vault client. No T3 gate. (R3-S2)

---

## 4. P2 — quality / robustness / fidelity

Grouped; each line keeps its source mapping.

**Validation & API design**
- **F19 · [K8S][T3]** — Add CEL `x-kubernetes-validations` (GA 1.29): make
  `sharding.bucketCount` immutable (`oldSelf` transition rule — changing it
  post-bootstrap is a data hazard); validate `spec.leader ∈ {<rs>-0..N-1}`; reject
  `shardingRoles` without cluster `sharding`; warn on `election` with `replicas < 3`
  / even counts (the 2-node election set bootstraps unreliably — empirically
  confirmed). (R4-I3, R3-I8, R3-I9)
- **F20 · [K8S]** — Add the condition triad `Available`/`Progressing`/`Degraded`
  (keep per-condition `observedGeneration`) so tooling distinguishes "converging"
  from "degraded but serving"; today only `Ready` exists. (R4-I2, R4-S1)
- **F26 · [K8S]** — Set up hub/spoke conversion before promoting the alpha API to
  beta/v1, so status/spec shape churn doesn't strand existing CRs. (R4-I4)

**Reconcile hygiene**
- **F21 · [K8S]** — (a) skip the status write when status is unchanged (today
  `Status().Update` runs every reconcile — needless API load); (b) build the config
  tree once (`Render` + `Hash` each call `buildTree`); (c) add a
  `GenerationChangedPredicate` to cut status-churn reconciles; (d) consider
  server-side apply (stable field manager) for pod-template fields co-owned with
  users. (R4-I6, R4-I7, R4-I8, R4-S10)

**Operator operability**
- **F22 · [K8S]** — Default to production (JSON) logging; `main.go` hardcodes
  `zap.Options{Development: true}`. (R4-I9)
- **F23 · [K8S]** — Bump stale deps: `controller-runtime v0.15.0` / `k8s.io/* v0.27.4`
  (~2 yrs old) → a current line (secure-by-default metrics, SSA/CEL tooling, fixes).
  (R4-I10)
- **F29 · [K8S]** — Register custom controller metrics (clusters reconciled, render/
  validation failures, leader-observation failures). (R4-S7)
- **F30 · [K8S]** — Run the operator Deployment with `replicas: 2` (leader-election is
  already enabled) for control-plane HA. (R4-S8)

**Security & network**
- **F24 · [T3][K8S]** — Emit an optional `NetworkPolicy` (peer + client + operator
  flows); document that on CE iproto is unencrypted (TLS + `pap-sha256` are EE-only) —
  use a mesh/sidecar if encryption is required. (R4-I22)

**Persistence**
- **F25 · [K8S][T3]** — Document the durability model (point `wal`/`snapshot`/`memtx`/
  `vinyl` dirs at the PVC; the leader's volume loss with unacked synchronous txns is
  the data-loss case), support PVC expansion, and steer production samples to PVCs
  (several use `emptyDir`). (R4-I19)

**Tarantool-3 fidelity niceties**
- **F27 · [T3]** — After each render/reload, read `config:info()` (`status`,
  `alerts`) over the console and surface it into Cluster/ReplicaSet status — a bad
  config becomes visible without reading pod logs. (R4-S6)
- **F28 · [T3]** — Expose election tuning the operator currently hides:
  `replication.election_mode` (`voter` for arbiter/external members),
  `synchro_quorum`, `election_fencing_mode` — they materially change HA safety.
  (R4-S2)
- **F31 · [T3]** — `memtx.memory` is grow-only at runtime; validate/warn that a
  decrease needs a restart and document vertical memory scaling as one-way without a
  restart. (R4-S9)
- **F32 · [T3]** — Evaluate adopting **`bootstrap_strategy: native`** (Tarantool
  ≥ 3.4, Jira TNTP-366) to drop the hardcoded `bootstrap_leader`. For `election`,
  `native` auto-selects the lowest-named instance (`<rs>-0`) — identical to the
  operator's current explicit choice — and for `off`/`manual` it
  `make_bootstrap_leader()`s the (last) RW instance, so a single bootstrap leader is
  guaranteed even with multiple RW instances. Gate on T3 ≥ 3.4 (keep `config` +
  `bootstrap_leader` for older images). **Caveat:** REVIEW.md RS1 tried `native` and
  saw a bootstrap deadlock for election — re-investigate against ≥ 3.4 before
  switching, since that contradicts the documented behavior. (new; corrects the
  earlier "native doesn't exist" finding)

---

## 5. Required-feature satisfiability (acknowledged separately)

Legend: ✅ supported · 🟡 partial (caveats/manual) · ❌ not supported. CE/EE = the
Tarantool edition the path requires. Ref = the F-item(s) that close the gap.

| # | Required feature | Status | CE/EE | Blocker / what it needs | Ref |
|---|---|---|---|---|---|
| 1 | Cluster creation | ✅ | CE | — | — |
| 2 | RPM/Lua app delivery (CentOS 7/8), multi-app | 🟡→❌ | CE | Multi-app topology works via per-replicaset `roles`; **no RPM/app delivery model**; needs a bundled image. | F4 |
| 3 | Configuration change | 🟡 | CE | Works, but **every change restarts pods** and file-watch reload doesn't fire — needs `config:reload()`/etcd + dynamic-key gating. | F2 |
| 4 | Horizontal / vertical scaling | 🟡 | CE | Works via STS + `autoexpel`; needs `/scale` (F13), PDB (F6), leader-aware rollout (F5); memtx grow-only (F31). | F5, F6, F13, F31 |
| 5 | Add host / shard | 🟡 | CE | Renders, but **vshard bootstrap/rebalance + weight-drain unmanaged**; needs vshard-bundled image. | F14, F4 |
| 6 | Join external replicas / federate | ❌ | CE | Topology closed to own pods; use `replication.anon` / external peers. | F15 |
| 7 | Health checks | 🟡 | CE | Readiness only; **no liveness/startup**, readiness too coarse. | F10 |
| 8 | Observability (dashboards + alerts) | ❌ | CE | Only a manual sample; no operator-managed ServiceMonitor/alerts/dashboard. | F16, F4 |
| 9 | Secret delivery from Vault | ❌ | CE | k8s Secrets only; needs CSI/external-secrets/Vault-Agent path. | F17 |
| todo | Resurrect failed replicas on a dead minion (1C) | ❌ | CE | Stuck pod on dead node; needs fence + force-reschedule + rejoin + PDB. | F3, F6 |
| (impl.) | Supervised / external failover coordinator | ❌ on CE | **EE** | `supervised` failover + its etcd stateboard are EE-only; CE HA path is `election` (≥3 voting members). | F28 |
| (impl.) | Encrypted iproto / strong auth | ❌ on CE | **EE** | iproto TLS + `pap-sha256` are EE-only; mitigate with NetworkPolicy/mesh. | F24 |

**Takeaway:** 1 of 9 features fully supported; 5 partial; 3 unsupported; the 1C todo
unaddressed. **Every requirement except supervised failover and in-process transport
encryption is achievable on Community Edition** with the current architecture — the
work is operator-side day-2 orchestration + integrations, not blocked by Tarantool.
The two EE gates should be called out to product as a conscious edition decision.

---

## 6. Traceability — every REVIEW-3 / REVIEW-4 item → F-ID (nothing dropped)

**REVIEW-3:** I1→F3 · I2→F4 · I3→F5 · I4→F6 · I5→F14 · I6→F2 · I7→F10 · I8→F19 ·
I9→F19 · S1→F4 · S2→F17 · S3→F16 · S4→F15 · S5→F5 · S6→F2 · S7→F3+F6 · S8→F13 ·
matrix→§5.

**REVIEW-4:** I1→F13 · I2→F20 · I3→F19 · I4→F26 · I5→F12 · I6→F21 · I7→F21 · I8→F21 ·
I9→F22 · I10→F23 · I11→F2 · I12→F1 · I13→F3 · I14→F6 · I15→F5 · I16→F7 · I17→F8 ·
I18→F9 · I19→F25 · I20→F10 · I21→F11 · I22→F24 · S1→F20 · S2→F28 · S3→F15 · S4→F14 ·
S5→F16 · S6→F27 · S7→F29 · S8→F30 · S9→F31 · S10→F21 · §4 satisfiability→§5.

(F-IDs are not contiguous because P2 items keep the numbers assigned during merge;
F1–F32 are all present across §2–§4. F18 was merged into F23 and intentionally left
unused. F32 is new — it replaces the earlier "`native` doesn't exist" claim with the
corrected ≥3.4 guidance.)

---

## 7. Prioritized execution order

1. **P0 (do first):** **F1** (autoexpel breaks bootstrap — confirmed via e2e, cheap
   fix, blocks everything multi-node), **F4** (app/module delivery + bundled image —
   the on-prem blocker), **F3**+**F6** (dead-minion remediation + PDB — the 1C todo
   and availability), **F2** (config change without restart).
2. **P1 (day-2 + integrations):** **F5** (leader-aware rollout) → unlocks safe
   **F9** (upgrades) and scaling; **F7** (graceful step-down), **F10** (probes),
   **F11** (pod securityContext), **F12** (RBAC), **F13** (`/scale`), **F14**
   (sharding lifecycle), **F8** (backup), then **F16** (observability), **F17**
   (Vault), **F15** (external replicas).
3. **P2 (quality):** F19–F31 (validation/CEL, conditions, reconcile hygiene, logging,
   deps bump, NetworkPolicy, persistence docs, config:info status, election tuning,
   operator HA, memtx warn).

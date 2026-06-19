# PLAN — Centralized config delivery (etcd / Tarantool Config Storage)

Status: **proposal / not started.** This plans the deferred "EE delivery backend"
from `AGENTS.md` Stage 2: deliver the rendered Tarantool 3 config through a
**centralized store** (external **etcd** or **Tarantool Config Storage / TCS**)
instead of a per-pod Secret-mounted file, so instances pull their config from the
store and **hot-reload** on change without a pod restart.

This document is implementation planning only — no code/CRD changes are made by it.

---

## 1. Goal, scope, and the EE caveat

**Goal.** Add an optional, per-Cluster *delivery backend* that publishes the
already-rendered config tree to a centralized store and points each instance at it
with a tiny local pointer config. The render engine (`pkg/clusterconfig`) is
unchanged; only how the bytes reach the instance changes.

**Why.** The current CE path (full config in a Secret, mounted as a file) restarts
pods for every config change because the file is baked into the pod template hash.
Centralized config gives Tarantool's native **watch + auto-reload**: republishing
applies dynamic changes live, and the operator only rolls pods for the few
*non-dynamic* keys. It also unlocks **`failover: supervised`**, whose coordinator
stores state in the same etcd/TCS prefix.

**⚠️ Edition gating (decision-driving, must verify first).** The Tarantool docs
state centralized configuration storages (both etcd and TCS) are **Enterprise
Edition only**:
> "Centralized configuration storages are supported by the Enterprise Edition only."
(etcd config page; introduced for etcd in 3.0, TCS by ~3.2.)

The operator *publishing* to etcd works with any image (it uses `go-storage`), but
a **community `tarantool/tarantool:3` instance reading from `config.etcd`/
`config.storage` is documented as EE-gated**. So this feature delivers value only
on EE images. The runtime code path ships in the open binary, so the gate may be
softer in practice — **Milestone 0 is to empirically confirm whether stock
`tarantool/tarantool:3.7` boots from a `config.etcd` pointer.** The design must:
- treat centralized delivery as opt-in, default off (CE Secret path stays default);
- fail fast with a clear status/event when selected against an image that can't
  consume it.

**Non-goals (v1 of this feature).**
- The operator does **not** run/operate an external etcd cluster — the user supplies
  endpoints (a managed/bundled etcd is a separate ops concern).
- Running a TCS cluster *as managed Tarantool replicasets by this operator* is a
  stretch goal (§11), not v1.
- Managing the supervised-failover coordinator process is out of scope (§12), though
  the design leaves room for it.

---

## 2. Current delivery (baseline to extend)

`ClusterReconciler.Reconcile` (controllers/tarantool3/cluster_controller.go):
1. list child ReplicaSets, resolve credential passwords from Secrets (own namespace);
2. `clusterconfig.Render(input)` → full config YAML; `clusterconfig.Hash(input)`;
3. reconcile headless Service;
4. `reconcileConfigSecret` — write rendered YAML into Secret `<cluster>-config`,
   gated on the `tarantool.io/config-hash` annotation (RP1 fix: no-op when unchanged).

`ReplicaSetReconciler` mounts that Secret at `/etc/tarantool/config.yaml`, sets
`TT_CONFIG`/`TT_INSTANCE_NAME`, and stamps the pod template with the config hash so a
change rolls the StatefulSet (`injectConfig` in resources.go).

**Key insight:** the renderer already emits one whole-cluster config tree. Centralized
delivery reuses that tree verbatim; it changes (a) *where the bytes go* and (b) *what
the pod's local config contains* (a pointer instead of the whole tree).

---

## 3. Tarantool facts that constrain the design

(Researched against tarantool.io docs + tarantool/tarantool changelogs, 3.0–3.7.)

- **Pointer config the pod carries** — only this, plus `TT_INSTANCE_NAME`:
  - external etcd:
    ```yaml
    config:
      etcd:
        endpoints: ['https://etcd:2379']
        prefix: /<cluster>          # must start with "/"
        username: <user>            # or TT_CONFIG_ETCD_USERNAME
        password: <pass>            # or TT_CONFIG_ETCD_PASSWORD
        ssl: { ca_file, ssl_cert, ssl_key, verify_peer, verify_host }
        http: { request: { timeout } }
        watchers: { reconnect_max_attempts, reconnect_timeout }
      reload: auto                  # default; 'manual' disables auto-reload
    ```
    Every key has a `TT_CONFIG_ETCD_*` env equivalent (so the pointer can be delivered
    purely via env, no file).
  - TCS (`config.storage`): per-endpoint objects with their own iproto `uri`/`login`/
    `password`, plus top-level `prefix`, `timeout`, `reconnect_after`.
- **Layout in the store:** full cluster config at `<prefix>/config/all`; Tarantool
  reads everything under `<prefix>/config/*` and merges. Instance selects its slice by
  its own name. `tt cluster publish <uri> config.yaml` is the CLI form; the operator
  will write the same key directly via `go-storage` (no `tt` at runtime).
- **Auto-reload:** with `config.reload: auto` (default) instances watch the prefix and
  hot-apply changes. Non-dynamic `box.cfg` keys (e.g. `iproto.listen`, `memtx.memory`
  decrease, data-dir paths, instance/replicaset UUIDs) still require a **restart** —
  dynamism is a property of the option, not the transport.
- **`go-storage` v1.5.0** (`github.com/tarantool/go-storage`, `go 1.25`): unified API
  over `driver/etcd` and `driver/tcs`; `Watch`, `Tx` with conditional predicates
  (CAS), value/version predicates, distributed locks, namespace `Prefixed` wrapper.
  Already noted in AGENTS.md as the reserved EE-backend lib.
- **Supervised failover** reuses the same store: config under `<prefix>/config/*`,
  coordinator state under `<prefix>/failover/*`.
- **etcd auth:** username/password and/or TLS client certs. **TCS auth:** a Tarantool
  user with `read,write` on spaces `config_storage`,`config_storage_meta` + `execute`
  on universe.

(Full citations in §16.)

---

## 4. Architecture — a pluggable delivery backend

Introduce a delivery abstraction so the reconciler is backend-agnostic. Keep the CRD→
tree mapping (`clusterconfig.Render`) untouched.

```
pkg/clusterconfig/        # unchanged: CRD -> config tree -> YAML + Hash
pkg/delivery/             # NEW
    delivery.go           # type Backend interface
    secret/               # existing behavior, refactored behind the interface (default)
    etcd/                 # go-storage driver/etcd publisher
    tcs/                  # go-storage driver/tcs publisher (stretch)
```

```go
// pkg/delivery/delivery.go
type Backend interface {
    // Publish makes `rendered` (hash `hash`) the active config for the cluster.
    // Idempotent: a no-op when the store already holds this hash.
    Publish(ctx context.Context, cluster *v2alpha1.Cluster, rendered []byte, hash string) error
    // PodConfig returns what each instance pod needs to FIND its config:
    // the pointer config bytes (or env) + how to mount/inject it.
    PodConfig(cluster *v2alpha1.Cluster) (PodWiring, error)
    // Cleanup removes the cluster's keys from the store (finalizer path).
    Cleanup(ctx context.Context, cluster *v2alpha1.Cluster) error
}
```

`ClusterReconciler` selects the backend from `cluster.Spec.ConfigDelivery.Mode`,
calls `Publish` in place of `reconcileConfigSecret` (the Secret backend's `Publish`
*is* today's `reconcileConfigSecret`). `ReplicaSetReconciler.injectConfig` consumes
`PodWiring` instead of hard-coding the Secret mount.

This refactor is valuable on its own (it cleanly separates render from deliver, as
AGENTS.md Stage 2 intended) and is **Milestone 1**, independent of etcd.

---

## 5. API changes (CRD `db.tarantool.io/v2alpha1`)

Add to `ClusterSpec` (additive, optional, default preserves today's behavior):

```go
// ConfigDelivery selects how the rendered config reaches instances.
// Defaults to mode "secret" (a mounted file, the CE-friendly path).
// +optional
ConfigDelivery *ConfigDelivery `json:"configDelivery,omitempty"`

type ConfigDelivery struct {
    // +kubebuilder:validation:Enum=secret;etcd;storage
    // +kubebuilder:default=secret
    Mode string `json:"mode"`

    // Etcd is required when mode=etcd.
    // +optional
    Etcd *EtcdDelivery `json:"etcd,omitempty"`

    // Storage is required when mode=storage (TCS).
    // +optional
    Storage *StorageDelivery `json:"storage,omitempty"`
}

type EtcdDelivery struct {
    Endpoints []string `json:"endpoints"`               // https://host:2379
    // Prefix in etcd; defaults to "/<namespace>/<cluster>".
    // +optional
    Prefix string `json:"prefix,omitempty"`
    // Username/password for etcd auth (Secret-backed).
    // +optional
    Auth *StoreAuth `json:"auth,omitempty"`
    // TLS for the etcd connection (Secret-backed CA/cert/key).
    // +optional
    TLS *StoreTLS `json:"tls,omitempty"`
}
// StorageDelivery mirrors EtcdDelivery for TCS (per-endpoint login/password).
```

Reuse the existing `SecretKeyReference` (now namespace-confined, per RP2) for
`StoreAuth`/`StoreTLS` — all secret material stays in the Cluster's namespace.

Validation (CEL / webhook-free): `mode=etcd ⟹ etcd set & endpoints non-empty`;
likewise for `storage`. Reject `mode != secret` when the operator can't reach the
store (surfaced as `ClusterError` at reconcile, since CEL can't check connectivity).

---

## 6. Publisher (etcd) implementation

In `pkg/delivery/etcd`:
- Open a `go-storage` etcd `Storage` from endpoints + resolved auth/TLS. Wrap with
  `Prefixed(<prefix>)`.
- `Publish`: write the rendered YAML to `<prefix>/config/all` using a **conditional
  transaction** keyed on a stored hash marker (e.g. `<prefix>/config/.operator-hash`):
  `If(ValueEqual(hashKey, hash)) Then(noop) Else(Put(all, rendered), Put(hashKey, hash))`.
  This makes Publish idempotent and races-safe across operator restarts/HA — the etcd
  analog of the RP1 hash gate.
- Connection caching: keep a per-Cluster client keyed by endpoints+auth fingerprint;
  close on change. Bound dial/op with a context timeout (mirror `observeBudget`).
- Errors are transient → `ClusterPending` + requeue (store may be briefly down), the
  same shape as the missing-credential-Secret path.

The full config still flows through `clusterconfig.Render` + `Hash`; the hash marker
in etcd is the same `clusterconfig.Hash`, so change detection is uniform across
backends.

---

## 7. Pod pointer config & instance bootstrap

For `mode=etcd`, pods must NOT mount the full config. Instead deliver the **pointer**:

- Render a tiny pointer config and put it in the existing `<cluster>-config` Secret
  (it carries etcd credentials → Secret is the right home), mounted at
  `/etc/tarantool/config.yaml` exactly as today; `TT_CONFIG` already points at it.
  `injectConfig` is unchanged except *what bytes* the Secret holds.
- Alternative: inject `TT_CONFIG_ETCD_*` env vars (endpoints/prefix/user) + mount only
  TLS material; no file. Decide in Milestone 2 — the Secret-pointer keeps `injectConfig`
  uniform and is preferred.
- `TT_INSTANCE_NAME` (already wired from `metadata.name`) selects the instance's slice.

So the pod-template wiring is nearly identical; the difference is the Secret content
(pointer vs full tree). **Important:** the pod-template config hash must now be the
hash of the *pointer* (which rarely changes), not the full config — otherwise we lose
the no-restart benefit. See §8.

---

## 8. Hot-reload vs rolling restart (the core payoff)

Today the pod template is stamped with the full-config hash, so any change rolls pods.
With centralized config:
- **Dynamic changes** (most of `box.cfg`, credentials, sharding weights, roles_cfg,
  log level, topology adds): republish to etcd → instances auto-reload. **No pod
  restart.** The pod template must therefore be stamped only with the **pointer hash**,
  which is stable across these changes.
- **Non-dynamic changes** (`iproto.listen` port, `memtx.memory` decrease, data-dir
  paths, UUIDs): require a restart. The operator must detect these and roll the
  StatefulSet.

Plan: split `clusterconfig.Hash` into two:
- `PointerHash` — over the delivery pointer (endpoints/prefix/port/mount) → stamped on
  the pod template (rolls only on pointer change).
- `ContentHash` — over the full tree → stored in etcd marker (drives republish).
- A `NonDynamicHash` — over the subset of keys known to require restart (a curated
  allow-list audited against the configuration reference "dynamic" column) → also
  stamped on the pod template, so non-dynamic edits still roll pods.

This directly mitigates **PROBLEMS P3** (membership change rolling the whole replica
set): adding an instance is a dynamic topology change → republish, no restart of the
existing pods (the new pod starts and joins).

---

## 9. Security

- **Store credentials** (etcd user/pass, TLS CA/cert/key; TCS user/pass) resolved from
  **Secrets in the Cluster's namespace only** (reuse the RP2-hardened
  `SecretKeyReference`, which no longer has a `Namespace` field).
- **In-config credentials exposure:** the etcd pointer embeds etcd credentials into the
  pod's config Secret — same trust boundary as today's rendered Secret (already holds
  Tarantool passwords). Document it; recommend etcd TLS + least-priv etcd user.
- **TCS minimal privileges:** a Tarantool user with `read,write` on `config_storage`,
  `config_storage_meta` + `execute` on universe.
- **Operator → store network:** the operator pod needs egress to etcd/TCS. Add the
  endpoints to any NetworkPolicy guidance; the operator must run **in-cluster**
  (the host `make run` dev loop may not reach etcd) — depends on the Dockerfile/deploy
  work already done.
- **RBAC:** no new k8s RBAC for etcd (it's external); Secret read is already granted.

---

## 10. Lifecycle: finalizers & cleanup (new requirement)

Owner-reference GC covers k8s objects but **not external etcd keys**. Centralized
delivery therefore *requires* a **finalizer** on `Cluster` (closes PROBLEMS **P9**
for this path):
- On delete: `Backend.Cleanup` removes `<prefix>/config/*` (and, if the operator ever
  manages it, `<prefix>/failover/*`), then the finalizer is removed.
- Guard against deleting a prefix shared with another cluster (prefix must be unique
  per cluster; default `/<ns>/<cluster>` guarantees this; reject user-set prefixes that
  collide).
- HA-safe writes via the conditional-txn hash marker (§6) so two operator replicas
  don't clobber each other.

---

## 11. TCS variant (stretch)

`mode=storage` reuses the same `Backend` via `go-storage` `driver/tcs`; only the
pointer shape (`config.storage.endpoints[]` objects) and auth (a Tarantool user)
differ. Two sub-options:
- **External TCS** the user runs — symmetric to etcd, low operator complexity.
- **Operator-managed TCS** — a dedicated `ReplicaSet` (or a `ConfigGroup`) with
  `roles: [config.storage]` that the operator stamps and the data clusters point at.
  Elegant (no external dependency) but introduces a bootstrap ordering problem (the
  config store needs config too) — defer past v1.

Recommendation: ship **etcd first** (mature, supervised-failover synergy, simplest
external dependency); add external TCS second; operator-managed TCS only if demanded.

---

## 12. Supervised-failover synergy (note, not v1)

Once etcd is wired, `replication.failover: supervised` becomes viable: the coordinator
reads/writes `<prefix>/failover/*` in the same etcd. Future work could let the operator
run the coordinator (`tarantool --failover`) as a small Deployment and grant the
`lua_call: [failover.execute]` privilege (3.2/3.3+). Track separately; the config-store
work is its prerequisite.

---

## 13. Testing strategy

1. **Unit (no cluster):** pointer-config rendering (etcd + TCS shapes), prefix
   defaulting/collision rejection, the hash split (`PointerHash`/`ContentHash`/
   `NonDynamicHash`), backend selection. Fake `go-storage` for `Publish`/`Cleanup`
   idempotency + the conditional-txn no-op.
2. **Integration vs real etcd (no k8s):** run an `etcd` container, publish via the
   backend, assert `<prefix>/config/all` content and the hash marker; flip a value and
   assert the CAS no-op/rewrite.
3. **envtest:** Cluster with `mode=etcd` → assert the pointer Secret content, the
   pod-template pointer hash is stable across content-only changes, finalizer added,
   `Cleanup` invoked on delete (fake/embedded etcd).
4. **kind e2e (EE-gated):** real etcd in-cluster + an EE Tarantool image; publish →
   instances boot from the pointer → change a dynamic key → assert it goes live with
   **no pod restart** (compare pod `startTime`); change a non-dynamic key → assert a
   roll. Mark the job to skip when no EE image/license is available (like the
   sharding-needs-vshard caveat). Add a `make test-e2e-etcd` target + `e2e.yml` matrix
   entry guarded on an EE-image secret.

---

## 14. Milestones

- **M0 — Spike/verify (½–1 day).** Confirm whether stock `tarantool/tarantool:3.7`
  (and an EE image, if available) boots from a `config.etcd` pointer against a real
  etcd. Decide the edition story. *Gates everything.*
- **M1 — Delivery abstraction (no behavior change).** Extract `pkg/delivery` + Secret
  backend behind `Backend`; reconciler calls it. Pure refactor, fully covered by
  existing tests.
- **M2 — etcd publisher + pointer + API.** `ConfigDelivery` CRD, `pkg/delivery/etcd`
  via go-storage, pointer Secret, CAS publish. Unit + real-etcd integration tests.
- **M3 — Hash split + no-restart reload.** `PointerHash`/`ContentHash`/`NonDynamicHash`;
  republish-without-roll for dynamic changes; roll only for non-dynamic. envtest.
- **M4 — Finalizer + cleanup + HA-safety.** Cluster finalizer, `Cleanup`, prefix-collision
  guard, conditional-txn writes.
- **M5 — kind e2e (EE-gated).** Real etcd + EE image; dynamic-vs-restart assertions.
- **M6 — TCS backend (stretch).** External TCS via go-storage `driver/tcs`.

M1 is independently shippable and de-risks the rest. M2–M5 deliver the feature.

---

## 15. Open questions / risks

- **EE gate (highest risk):** does centralized config actually require an EE binary at
  the target version? M0 must answer empirically; if EE-only, the feature's audience is
  EE users and CI e2e needs an EE image (license/secret).
- **Dynamic-key catalog:** the `NonDynamicHash` allow-list must be audited per target
  T3 minor (the "dynamic" column drifts across 3.x). Wrong classification → either
  missed restarts (stale instances) or needless rolls.
- **Pin the TCS intro version** (3.1.x vs 3.2) and the `lua_call`-for-failover version
  (3.2 vs 3.3) before relying on them — sources disagreed.
- **etcd ownership:** operator does not run etcd; users must provide HA etcd. Document
  sizing/HA expectations; a broken etcd makes the whole cluster unconfigurable.
- **Mixed delivery / migration:** switching an existing Cluster from `secret`→`etcd`
  changes what pods mount → a one-time roll. Define the transition (publish first, then
  swap the pointer, then roll) so there's no window with neither source.
- **Interaction with RP6 (manual replication not forming):** orthogonal — it's a render
  bug fixed in `clusterconfig` regardless of backend; centralized delivery neither
  causes nor fixes it.

---

## 16. References

- Centralized config (etcd/TCS), EE gating, publish, watch/reload:
  https://www.tarantool.io/en/doc/latest/platform/configuration/configuration_etcd/
- Configuration reference (`config.etcd`/`config.storage`/`config.reload`, dynamic column):
  https://www.tarantool.io/en/doc/latest/reference/configuration/configuration_reference/
- Supervised failover (coordinator state under `<prefix>/failover/*`, EE):
  https://www.tarantool.io/en/doc/latest/platform/replication/supervised_failover/
- 3.0 release (etcd centralized config, EE): https://www.tarantool.io/en/doc/latest/release/3.0.0/
- 3.1.1 (`etcd ssl.ssl_cert`): https://github.com/tarantool/tarantool/releases/tag/3.1.1
- 3.2 (TCS TTL, EE): https://www.tarantool.io/en/doc/latest/release/3.2.0/
- go-storage (etcd + TCS drivers, watch/txn/predicates/locks), v1.5.0:
  https://github.com/tarantool/go-storage
- AGENTS.md — Stage 2 "Delivery (EE — deferred)"; go-storage reserved as the EE backend.

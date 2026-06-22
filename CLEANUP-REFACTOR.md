# CLEANUP-REFACTOR — Tarantool 3 operator: maintainability & minimalism

Status: **executed** (every item except those the maintainer declined — see
per-item markers). Maintainer decisions: keep the legacy Cartridge tree (R1/R2
declined), keep all tests and e2e suites (R5 declined), remove rather than
archive the finished docs (R6).

Two analyses in one file, same goal — **a codebase that is easy to support, as
minimalistic as possible**:

- **Part I (R-series)** — code that can be **removed** because it is unused or
  almost unused. Every claim verified with whole-program analysis
  (`golang.org/x/tools/cmd/deadcode`), import-graph greps, and caller counts —
  not assumed.
- **Part II (C-series)** — the earlier structural analysis (duplication,
  seams, readability) of the code that stays. Still valid; statuses updated.

> ⚠ This area is under active parallel development. Each item names the files it
> touches; land items as separate commits and re-verify against HEAD before each.

## Where the lines actually are

| Tree | Prod LOC | Test LOC | Reachable from `main()`? |
|---|---:|---:|---|
| `pkg/cartridge` | 3158 | 596 | yes (legacy reconcilers) |
| `apis/cartridge` | 1672 | 0 | v1beta1 yes; **v1alpha1 no — zero importers** |
| `controllers/tarantool3` | 1498 | 1968 | yes |
| `pkg/clusterconfig` | 930 | 1214 | yes |
| `internal/cartridge` | 654 | 0 | yes (legacy) |
| `apis/v2alpha1` | 655 | 0 | yes |
| `test/cartridge` (mocks/fixtures) | 532 | 0 | **no — every symbol dead** |
| `controllers/cartridge` | 459 | 335 | yes (legacy) |

The Tarantool 3 operator is ~3.1k prod LOC. The legacy Cartridge operator is
~6.5k prod LOC — **two thirds of the Go code maintains the deprecated product**.
No structural refactor of the T3 code can buy what one decision about the legacy
tree buys.

---

# Part I — Removal plan (R-series)

## R1 — Drop the legacy Cartridge operator ✗ declined (keep Cartridge)

**The headline item.** Everything below is verified reachable *only* from the
three legacy reconcilers registered in `main.go` (`clusterReconciler`,
`roleReconciler`, `cartridgeConfigReconciler`) and the `v1beta1` scheme:

- `apis/cartridge/` (1672), `controllers/cartridge/` (459 + 335 test),
  `internal/cartridge/` (654), `pkg/cartridge/` (3158 + 596 test),
  `test/cartridge/` (532 — already 100% dead, see R2).
- `config/crd/bases/cartridge/` (4 CRDs), `config/samples/cartridge/`,
  the cartridge paths in the Makefile `manifests`/`generate` targets.
- **7 direct go.mod dependencies used by nothing else**: `pkg/errors`,
  `google/uuid`, `onsi/ginkgo`, `stretchr/testify`, `google/go-cmp`,
  `go-logr/logr` (as a direct dep), and most of `onsi/gomega` (only 2
  non-cartridge files use it — the T3 envtest suite keeps it).

**Why it's safe to consider now:** the Cartridge CRDs live in a different API
group (`tarantool.io` vs `db.tarantool.io`) — deleting the code does not touch
existing Cartridge clusters' CRs, and the published 1.x operator images keep
working for them. Git history preserves everything; the tree was already
segregated by relocation for exactly this moment.

**What it needs:** a maintainer call on the AGENTS.md Stage-5 "transition
window" (this repo's `main` has not shipped the combined binary yet — if the
T3 operator releases as 2.0 from a clean tree, the window can be the 1.x branch
instead of dual code in HEAD).

**Impact: −~7.4k Go LOC (−66% of the codebase), −7 deps, −4 CRDs, main.go
shrinks to two reconcilers.** Everything else in this file is an order of
magnitude smaller.

## R2 — Delete the fully-dead leaves ✗ declined (Cartridge stays untouched)

Whole-program `deadcode` analysis; zero callers/importers anywhere, including
tests:

| What | LOC | Evidence |
|---|---:|---|
| `apis/cartridge/v1alpha1/` (whole package) | 618 | imported by **no Go file**; not in any scheme; `config/samples/cartridge/tarantool.io_v1alpha1_*.yaml` reference it but nothing serves it |
| `test/cartridge/` (mocks, resources, utils) | 532 | every symbol `unreachable` — the Ginkgo suite that used `FakeCartridgeTopology` was deleted long ago |
| `pkg/cartridge/utils/pods.go` (IsPodReady family) | ~45 | unreachable even from the legacy reconcilers |
| `pkg/cartridge/utils/slice.go` (`SliceContains`) | ~8 | same |

Also regenerate after the v1alpha1 deletion: `config/crd/bases/cartridge/*`
shed their v1alpha1 versions; drop the two v1alpha1 sample YAMLs +
`config/samples/cartridge/kustomization.yaml` entries.

**Impact: −~1.2k LOC, zero risk, independent of the R1 decision** (it's a
strict subset — do it first so R1 stays a clean one-commit delete).

## R3 — v2alpha1 dead API surface ✅ done (extends old C1)

Caller-count audit of every helper on the v2alpha1 types:

| What | Callers | Action |
|---|---|---|
| `Cluster.SetPhase/GetPhase/SetLeader/GetLeader` | 0 | delete (controllers write `Status` fields directly) |
| `ReplicaSet.SetPhase/GetPhase` | 0 | delete (`setStatus` assigns `rs.Status.Phase` directly) |
| `Cluster.Status.Leader` field | never written | delete — leadership is per-replicaset by design; dead API surface invites misuse |
| `Failover` printcolumn (`cluster_types.go`) | — | `JSONPath=".spec.replication.failover"` — the path no longer exists (config-first move); column is permanently empty. Repoint is impossible (free-form JSON) → delete |
| `Leader` printcolumn (Cluster) | — | reads the never-written field → delete with it |
| keep: `GetDomain` (4), `GetReplicas` (14), `InstanceName` (12), `GetGroup` (10+), `SetReadyInstances` (1) | | |

Needs `make generate manifests` (deepcopy + CRD regen) in the same commit.

**Impact: −~50 LOC, −2 misleading kubectl columns, −1 dead status field.**

## R4 — pkg/clusterconfig: one render entry point ✅ done

`deadcode` confirms production reaches only `RenderAndHash`; `Render` and
`Hash` are test-only entry points:

- `Hash(in Input)` (`hash.go`): no non-test callers. Move `hashTree` into
  `render.go`, delete `hash.go`, port the hash tests to `RenderAndHash`.
- `Render`: keep (the external test package needs one render entry and the
  golden tests are the regression net), but it becomes *the* documented pair:
  `Render` for tests, `RenderAndHash` for controllers.

**Impact: −1 file, −~25 LOC, 3 exported entry points → 2.**

## R5 — e2e suite dedup ✗ declined (tests stay)

The kind ladder grew a suite per feature; two are now strict coverage subsets
of `kind-stack.sh` (large replicated cluster **+** application role **+** Lua
app in one fixture):

| Suite | LOC | Unique coverage vs `stack` |
|---|---:|---|
| `kind-large.sh` + `large.yaml` | 110+ | only the 5-replica-set count (stack has 3) |
| `kind-luaapp.sh` + `luaapp.yaml` | 94+ | none (stack loads `app.file` on storages) |

Both also run as separate jobs in `.github/workflows/e2e.yml`'s matrix.
Dropping them: −2 CI jobs (~10 min each), −~300 lines of script+fixture, and
the matrix keeps smoke/scaling/config/roles/stack (+ reload/leader/upgrade/
nodeloss locally). If the 5-RS topology is considered worth keeping, fold a
fourth replica set into `stack.yaml` instead — one fixture to maintain.

## R6 — point-in-time docs ✅ done

`REVIEW.md` (superseded by `REVIEW-FINAL.md`), `PROBLEMS.md` (fix-log, all
closed), and `REFACTOR.md` (status: implemented) were finished snapshots that
no longer guided work — removed per the maintainer's call (git history keeps
them). `REVIEW-FINAL.md` stays: it is the live F-series backlog.

---

# Part II — Structural plan for what stays (C-series)

## C1 — Delete dead code ✅ done

`toAnySlice`, `roleSuper`, `submapFromAny` removed (commit `1455425`, lint
clean). The remaining C1 items are superseded by **R3** (API surface) and
**R4** (`Hash`/`hash.go`).

## C2 — One “delivered config” object instead of five parse-the-YAML entry points ✅ done

`pkg/clusterconfig` exposes five functions that each `yaml.Unmarshal` the same
rendered config bytes: `ContainsReplicaSet`, `IntrospectSettings`,
`InstanceScopeHash`, `RolloutHash`, `MemtxMemory`. The ReplicaSet reconciler calls
**three of them per reconcile** — three parses of the same Secret payload — and
the Cluster reconciler parses twice more for the memtx warning.

**Proposal:** a single parse, one type:

```go
d, err := clusterconfig.ParseDelivered(secret.Data[ConfigFileName])
d.ContainsReplicaSet(group, rs)
d.Settings(group, rs)        // incl. SuperUser since the credentials move
d.InstanceScopeHash(group, rs)
d.RolloutHash(group, rs)
d.MemtxMemory()
```

`scope_hash.go` + `introspect.go` collapse into one `delivered.go`. The free
function `ContainsReplicaSet` also hand-rolls nested-map walking that `lookup`
already implements — unify on `lookup`.

**Impact:** 5 exported funcs → 1 type; 3-5 parses/reconcile → 1; one obvious place
to add the next introspection need. −~80 LOC.

## C3 — One iproto seam instead of three ✅ done

Three near-identical "connect over iproto, eval, return first string" functions,
each its own package-var test seam: `readElectedLeader` (status.go),
`reloadInstanceConfig` (status.go), `evalInstanceString` (upgrade.go — already
the generic shape).

**Proposal:** keep only `evalInstanceString` (rename `evalString`) as **the** seam;
the other two become two-line wrappers building the script. Tests stub one
variable instead of three. Same cluster: the instance-address `fmt.Sprintf` is
built in three controller sites — use `instanceAddr` everywhere; and the
per-instance loop with an `observeBudget` context appears three times — a tiny
`forEachInstance` iterator unifies budget semantics.

**Impact:** −~70 LOC; one network seam; uniform timeout behavior.

## C4 — Failover-mode string literals → shared constants ✅ done

`"off"` / `"manual"` / `"election"` / `"supervised"` appear as raw literals at 9
sites across `remediation.go`, `resources.go`, `status.go`, and the renderer —
these gate split-brain protections; a typo silently changes behavior.
Constants in `clusterconfig` (`FailoverOff/Manual/Election/Supervised`), plus one
`DefaultIprotoPort` for the 5 hardcoded `3301`s.

**Impact:** typo-proof gates; −5 magic numbers.

## C5 — Unify the two config-precedence readers ✅ done

Two parallel implementations of "read an effective value with Tarantool scope
precedence": pre-render `userCfg` (walks CR JSON; has `listenPort`,
`userWithRole`) and post-render `IntrospectSettings` (walks delivered YAML;
re-implements the same precedence, `userWithRole`, and port extraction — the
credentials move added the `SuperUser` lookup to *both* sides). They must agree
forever; today that agreement is by parallel maintenance.

**Proposal:** one precedence helper over `map[string]any` scopes —
`scopeChain{inst, rs, group, global}.value(path…)` — used by both `userCfg` and
the `Delivered` type from C2. `userWithRole` and port extraction exist once.

**Impact:** strongest single maintainability win after R1/C2; renderer and
introspector can no longer drift. −~50 LOC.

## C6 — Straighten `ReplicaSetReconciler.Reconcile` and the status contract ✅ done

1. `Reconcile` is ~130 lines of appended concerns. Extract the post-STS block
   into `r.runtimeOps(ctx, c, rs, settings, ready)` — leader observation,
   reload, schema upgrade, remediation are all "talk to the running cluster"
   operations with the same inputs.
2. `setStatus` takes 5 positional args *and* silently persists fields mutated
   earlier (`Status.Replicas`, `Bootstrapped`, `SchemaUpgradedForImage`). Make
   it one `observed` struct passed to a single `applyStatus` that owns all
   status writes.
3. Rename `desiredHash`/`configHash` → `deliveredHash`/`rolloutHash`.

**Impact:** Reconcile reads as five named phases; status writes have one owner.

## C7 — Small hygiene ✅ done (prod-code parts; test-file merges skipped — tests untouched)

- Inline `readyCondition` (trivial wrapper, 2 callers).
- Drop the 4 `Recorder != nil` checks; use `record.NewFakeRecorder` in bare
  test constructors.
- After C3, split the `status.go` misnomer: `conditions.go` + `iproto.go`.
- One generic `upsert[T]` for `upsertVolume`/`upsertVolumeMount`/`upsertEnv`.
- Merge `resources_test.go` + `resources_more_test.go`; fold the 26-line
  `leaderobs_test.go` into the iproto test file.

---

## Explicitly NOT proposed (considered and rejected)

- **No step/pipeline framework** for reconcilers (the Cartridge
  `SteppedReconciler` next door is the cautionary tale — and under R1 it
  leaves the repo entirely).
- **No interface extraction** for the k8s client or reconcilers — package-var
  seam + envtest covers testing.
- **No splitting `controllers/tarantool3` into subpackages** at ~1.5k LOC.
- **No reintroduction of typed spec fields** — config-first (incl. the
  credentials move) is the right minimalism; C5 makes its one cost cheap.
- **No deleting `Render`** from the test surface (R4 keeps it deliberately).

---

## Suggested landing order

| # | Item | Risk | Effort | LOC |
|---|---|---|---|---:|
| 1 | R2 fully-dead leaves (v1alpha1, test/cartridge, utils) | none | XS | −1.2k |
| 2 | R3 v2alpha1 dead API (+ CRD regen) | none | XS | −50 |
| 3 | R4 hash.go fold | none | XS | −25 |
| 4 | C4 failover/port constants | none | XS | — |
| 5 | C7 hygiene batch | none | S | −40 |
| 6 | C3 one iproto seam + addr + loop | low | S | −70 |
| 7 | C2 `Delivered` type | low | M | −80 |
| 8 | C5 unified precedence reader | medium (golden tests guard) | M | −50 |
| 9 | C6 Reconcile/status structure | medium (envtest + e2e guard) | M | — |
| 10 | R5 e2e dedup | low (CI only) | S | −300 script |
| 11 | R6 docs archive | none | XS | — |
| — | **R1 Cartridge removal — pending maintainer decision** | product call | S (mechanical) | **−7.4k** |

Without R1: roughly **−1.5k LOC** plus the structural wins. With R1: **−~9k
LOC**, 7 fewer dependencies, and the repository *is* the Tarantool 3 operator.
All items behavior-preserving for `db.tarantool.io/v2alpha1`; regression net =
unit + envtest after every item, one kind e2e (`make test-e2e`) after C5/C6,
the full ladder before R1 ships.

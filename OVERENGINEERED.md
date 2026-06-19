# Overengineered code in the Tarantool 3 operator

A focused pass over the `db.tarantool.io/v2alpha1` operator
(`controllers/tarantool3/`, `pkg/clusterconfig/`, `apis/v2alpha1/`) looking only
for **overengineering** — abstraction that costs more than it pays: speculative
generality, indirection with one caller, machinery heavier than its need, knobs
that are never varied. This is *not* a bug hunt and *not* a style review.

Each finding lists `file:line`, what the construct is, the concrete evidence it
is overengineered, and a simpler alternative. A deliberately large section at the
end — **"Looks heavy but is earned"** — records complexity that *is* justified, so
nobody "simplifies" load-bearing code by mistake.

Claims were verified against the source (call counts, dependency usage, caller
behaviour), not assumed.

---

## Findings

### 1. The whole `go-config` dependency is used only to marshal a map to YAML — with validation turned off  ·  HIGH

`pkg/clusterconfig/render.go:138-154` (`marshalTree`)

```go
func marshalTree(tree map[string]any) ([]byte, error) {
	b := goconfig.NewBuilder()
	b = b.AddCollector(collectors.NewMap(tree).WithName("operator"))
	b = b.WithoutValidation()
	cfg, errs := b.Build(context.Background())
	...
	return cfg.MarshalYAML()
}
```

This is the **only** non-test use of `github.com/tarantool/go-config` in the repo
(verified: the import appears solely in `render.go`). The code stands up the
library's full builder/collector/`Build()` pipeline purely to turn a
`map[string]any` into YAML — and calls `.WithoutValidation()`, switching off
schema validation, which is the one feature that justifies pulling in a config
library at all. So the operator pays for a dependency (and its transitive tree)
to do the job of a one-liner, while explicitly disabling the part that isn't a
one-liner.

`sigs.k8s.io/yaml` is **already** a direct dependency (`delivered.go` uses it to
*parse* the rendered config). `sigs.k8s.io/yaml.Marshal(tree)` produces an
equivalent YAML document — Tarantool does not care about key order, and change
detection already hashes a separate canonical JSON form (`hashTree`), so output
byte-stability is irrelevant here.

**Why it's overengineered:** maximum dependency surface, minimum benefit — the
library is invoked only in the mode where it does nothing the standard library
can't.

**Simpler alternative:** replace `marshalTree`'s body with
`yaml.Marshal(tree)` (the `sigs.k8s.io/yaml` already imported in the package) and
drop the `go-config` import. Validate that the rendered output still loads in
Tarantool, then remove `go-config` from `go.mod` (it also drops whatever
transitive weight — e.g. centralized-config-storage clients — go-config brings).

**Caveat (read before acting):** the code comments and package doc state a
*future* rationale — "a path to JSON-Schema validation later, and
centralized-storage delivery." That is exactly the YAGNI shape (carry the cost
now for a maybe-later feature), but it is a deliberate choice, and this repo's
stated preference is to favour official `tarantool/*` libraries even when heavy.
So treat this as: *if* schema validation / config-storage delivery is on the
near roadmap, keep `go-config` but drop the `WithoutValidation` builder dance
once validation is actually wanted; *otherwise* this is the single biggest
"abstraction with no current payoff" in the operator.

---

### 2. `upsertByName` generic + three one-line wrappers for four call sites  ·  MEDIUM

`controllers/tarantool3/resources.go:294-317`

```go
func upsertByName[T interface {
	corev1.Volume | corev1.VolumeMount | corev1.EnvVar
}](items []T, item T, name func(T) string) []T { ... }

func upsertVolume(vols []corev1.Volume, v corev1.Volume) []corev1.Volume {
	return upsertByName(vols, v, func(x corev1.Volume) string { return x.Name })
}
func upsertVolumeMount(...) { return upsertByName(...) }
func upsertEnv(...)         { return upsertByName(...) }
```

A type-parameterised "upsert" with a union constraint and a `name func(T) string`
extractor, plus three wrappers that exist only to bind that extractor to
`.Name`. Verified call counts: `upsertVolume` ×1, `upsertVolumeMount` ×1,
`upsertEnv` ×2 — all inside `injectConfig`/`injectProbes` in the same file. So
the generic machinery serves four call sites, and the three wrappers each pin the
same trivial `func(x) string { return x.Name }`.

**Why it's overengineered:** the generic + union constraint + closure extractor
is more apparatus than the problem ("replace-or-append by `.Name`") warrants for
four in-file uses. The wrappers add a layer that only re-supplies `.Name`.

**Simpler alternative:** keep one small helper per type (three ~5-line
`for`-loops), or — since all callers are in one file — a single non-generic
`upsertEnv`-style helper and inline the volume/mount cases. Either removes the
union constraint, the extractor parameter, and two of the wrappers.

---

### 3. `bootstrapOutcome` is a three-value enum where the caller distinguishes only one value  ·  MEDIUM

`controllers/tarantool3/vshardbootstrap.go:34-39`, caller at
`controllers/tarantool3/replicaset_controller.go:320`

```go
type bootstrapOutcome int
const (
	bootstrapNoop  bootstrapOutcome = iota // nothing to do
	bootstrapDone                          // bootstrapped this pass
	bootstrapRetry                         // storages not ready yet
)
...
// the ONLY caller:
if isRouter && r.maybeBootstrapVshard(...) == bootstrapRetry {
	requeueAfter = shardBootstrapRequeue
}
```

The caller only ever asks "was it `bootstrapRetry`?". `bootstrapDone` and
`bootstrapNoop` are indistinguishable to every caller — and the side effect that
`bootstrapDone` documents (setting the sticky `Status.ShardingBootstrapped`
flag) happens *inside* `maybeBootstrapVshard`, not in response to the return
value. So two of the three enum values carry no information across the function
boundary.

**Why it's overengineered:** a three-state type models a distinction the program
never observes.

**Simpler alternative:** return `bool` (`true` ⇒ "retry needed / requeue"), drop
the enum and its three constants. The function keeps setting the status flag
internally exactly as now.

---

### 4. Minor: one-caller predicate helpers

These are small and partly a matter of taste, but each is an extracted helper
with a single production caller, where inlining would make the caller
self-contained:

- `controllers/tarantool3/configerror.go:35-44` `lineIsConfigError` — one caller
  (`extractConfigRejection`, same file). The signature loop could fold into the
  line scan.
- `controllers/tarantool3/status.go:230-238` `needsNetworkLeaderObservation` —
  one production caller. A two-arm `switch` on the failover mode could sit at the
  call site with a comment.

Low value either way; listed for completeness, not as a priority.

---

## Looks heavy but is earned (do **not** "simplify")

Several constructs are genuinely complex but the complexity is load-bearing —
each pays for itself, usually against a documented failure mode. Flagging them so
they are not mistaken for the findings above.

- **Dual config hashing + scope-strip machinery** —
  `pkg/clusterconfig/delivered.go:189-309` (`InstanceScopeHash` vs `RolloutHash`,
  `dynamicScopePaths`, `alwaysStripPaths`, `stripGeneratedInstanceWiring`,
  `deletePath` empty-parent pruning). This is the densest code in the operator,
  but every piece exists to stop an unnecessary pod roll on a hot-reloadable
  change (scaling, role add, `roles_cfg`, `memtx.memory`, …), each tied to a real
  bug noted in the comments. Earned.

- **`PasswordSource` interface with a single (Vault) implementation** —
  `controllers/tarantool3/credentials.go:11-29` + `credentials_vault.go`. A
  one-implementation interface is normally a smell, but here it is the mechanism
  that keeps Vault **entirely out of the default build**: `credentials_vault.go`
  is `//go:build vault` and registers itself via `init()` into the
  `passwordSources` slice, so the untagged core never names a tagged symbol. The
  seam is what makes the build-tag isolation possible — not speculative.

- **`forEachInstance` time budget** — `controllers/tarantool3/iproto.go`. The
  shared `observeBudget` caps how long an instance sweep can stall a reconcile
  (otherwise N×`connectTimeout`). Load-shedding infrastructure, justified.

- **The `observed` struct + `condition()`/status-condition builders** —
  `replicaset_controller.go` / `status.go`. Normal reconcile plumbing; `condition`
  has ~8 callers and the builders encapsulate non-trivial reason/quorum logic.
  Reuse, not over-abstraction.

- **`reloadRetryTotal` / `leaderObservationFailuresTotal` metrics** —
  `controllers/tarantool3/metrics.go`. These are registered to
  `metrics.Registry` (the controller-runtime global registry) and so are exported
  on `/metrics`; "no dashboard yet" is not "dead code." Fine.

- **`tarantoolContainerIndex` fallback** — `resources.go`. The name-or-index-0
  lookup handles both named and single-container pod templates; its `-1` path is
  cheap defensiveness. Not worth changing.

---

## Summary

| # | Location | Construct | Fix | Confidence | Status |
|---|----------|-----------|-----|-----------|--------|
| 1 | `render.go:138` | whole `go-config` dep used only to `MarshalYAML` with validation off | `yaml.Marshal` (dep already present); drop `go-config` | HIGH (mind the future-validation caveat) | **FIXED** — switched to `sigs.k8s.io/yaml`; `go-config` and 13 modules (the `jsonschema`/`goccy-yaml`/`go-storage` stack, −141 lines of `go.mod`/`go.sum`) removed. Verified: rendered config loads in Tarantool and a 3-instance election cluster forms + hot-reloads. |
| 2 | `resources.go:294-317` | generic `upsertByName` + 3 wrappers for 4 calls | per-type helpers / inline | MEDIUM | **FIXED** — replaced with three self-contained per-type helpers (no generic, no extractor closures). |
| 3 | `vshardbootstrap.go:34` | 3-value `bootstrapOutcome`, caller uses 1 | return `bool` | MEDIUM | **FIXED** — returns `retry bool`; enum + 3 constants removed; caller and test updated. |
| 4 | `configerror.go:35`, `status.go:230` | one-caller predicate helpers | inline | LOW | **WON'T FIX** — on inspection these are well-named predicates that flatten their callers (and `needsNetworkLeaderObservation` has a documented default + test coverage); inlining nests a loop / duplicates a switch into the caller, i.e. makes the code worse. |

The highest-leverage change was **#1** — it removed an entire dependency (and
its transitive JSON-Schema validation tree) for a standard-library one-liner. #2
and #3 were small, safe local cleanups. #4 was reconsidered and deliberately left
alone. Everything in "Looks heavy but is earned" was confirmed and untouched.

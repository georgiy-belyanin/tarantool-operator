# Test validation — problems found

A skeptical, one-by-one validation of the operator's tests. The goal was not
"do they pass" (they do) but "can they *fail* — do they actually catch a
regression?" Every claim below was verified empirically, not assumed.

## Method

1. **Ran the whole suite for real.** Provisioned envtest control-plane binaries
   (`setup-envtest use 1.31.0`) and ran `go test ./...` with `KUBEBUILDER_ASSETS`
   set, plus `-race`. Result with envtest present: **72 pass, 0 skip, 0 fail**,
   no data races.
2. **Mutation testing.** For each suspected weak spot, introduced a one-line bug
   into the *production* code, ran the relevant test, and recorded whether it was
   **caught** (test failed — good) or **escaped** (test still passed — gap), then
   reverted. This is the only honest way to validate a test.
3. **e2e + CI audit.** Read all 23 `test/e2e/kind-*.sh` scripts for
   failure-swallowing and silent-pass patterns, and diffed the Makefile e2e
   targets against what the GitHub workflows actually run. (See the e2e section.)

Headline: **the suite is much stronger than a static read suggests.** Of eight
plausible "this can't be caught" hypotheses, mutation testing **debunked six** —
the tests caught the bug. The genuine gaps it found (P2–P5, plus the E1 CI hole)
have since been **fixed and re-verified by mutation**; P1 is a CI-safe local
footgun (softened), and P6 is a latent isolation risk left documented. Each
"FIXED" below was confirmed by re-running the escaping mutation and seeing the
strengthened test now fail.

---

## Confirmed problems (verified by mutation or by execution)

### P1 — A bare `go test` silently *skips* the envtest suite  ·  LOW–MEDIUM (local footgun; CI is OK)

`suite_test.go:45-49,106-118`. `TestMain` checks `KUBEBUILDER_ASSETS`; when it's
unset it calls `runSkipped`, which makes every integration test `t.Skip(...)`.
A skipped test still counts toward a green `ok`. So a bare
`go test ./controllers/tarantool3/...` on a dev box **runs 12 integration tests as
no-ops and reports success.** I hit exactly this: the first runs this session
printed `ok` while the integration tests never executed; only after
`setup-envtest` did 72 tests actually run.

*Correction after checking CI:* `.github/workflows/test.yml` **does** provision
envtest (`setup-envtest use 1.31.0` → `KUBEBUILDER_ASSETS` in `$GITHUB_ENV`), so
the integration tests **do** run in CI. This is therefore a *local-developer*
footgun, not a CI hole — downgraded from the HIGH I first assigned.
*Evidence:* assets unset → `--- SKIP` but exit 0; assets set → 72 PASS / 0 SKIP;
`test.yml` lines 25-31 install and export the assets.
*Fix (optional):* have `runSkipped` print a loud one-line banner so a local run
makes the skip obvious, and document that `make test` (which provisions assets) is
the validating entrypoint.

> **Update:** P2, P3, P4, P5 fixed (and the P1 footgun softened). Each fix was
> verified by re-running the escaping mutation and confirming the strengthened
> test now **fails** (catches it). The fixes touch only test files plus a one-line
> banner in `suite_test.go`; no production behavior changed.

### P2 — Readiness/liveness probe parameters are not asserted  ·  MEDIUM  ·  FIXED

`resources_test.go:117-123` checks only that the probe *command* contains
`box.info.status` and `tt connect`. Nothing asserts `FailureThreshold`,
`PeriodSeconds`, `InitialDelaySeconds`, `SuccessThreshold`, `TimeoutSeconds`.

*Evidence (mutation M8):* changed `FailureThreshold: 3` → `1` in `resources.go`
— **suite still passed.** A probe that fails a pod on the first missed check
(killing healthy-but-busy instances) would ship undetected.
*Fixed:* added `TestReadinessProbeParameters` (`probe_test.go`) asserting
`InitialDelaySeconds=5, PeriodSeconds=10, TimeoutSeconds=5, FailureThreshold=3`.
Verified: the `FailureThreshold: 3 → 1` mutation now fails the test.

### P3 — The "idempotency" test doesn't exercise the hash gate it implies  ·  MEDIUM  ·  FIXED

`reconcile_idempotency_test.go` (`TestConfigSecretStableAcrossReconciles`) waits
for the config Secret's `ResourceVersion` to stop changing, then asserts it stays
put. That proves "things settled," not "a redundant write is suppressed."

*Evidence (mutation M10):* replaced the gate
`if secret.Annotations[ConfigHashAnnotation] != hash || secret.Data[...] == nil`
with `if true` (always rewrite) — **suite still passed.** (Post the
`sigs.k8s.io/yaml` switch the render is byte-deterministic, so an unconditional
write produces identical bytes and `CreateOrUpdate` no-ops — which is *why* the
test can't see the difference. The gate still earns its keep for the memtx-shrink
check and to avoid re-parsing, but the test does not validate it.)
*Fixed:* added `TestConfigSecretMemtxShrinkWarns` (`configsecret_test.go`), a
direct unit test of `reconcileConfigSecret` that drives the gated path: a
memtx.memory **decrease** must emit `MemtxShrinkRequiresRestart`, a grow/steady
must not. (Pure idempotency is now guaranteed by the deterministic render, so the
gate's one *distinct* behavior — the shrink warning — is what's worth pinning, and
it was untested.) Verified: inverting the `newMem < oldMem` comparison now fails
the test. The three existing idempotency/scope tests are kept.

### P4 — `configerror` signature matching is under-tested  ·  LOW–MEDIUM  ·  FIXED

`configerror_test.go` (`TestExtractConfigRejection`). Two sub-gaps:

- **Redundant signature.** Every positive case that involves `[cluster_config]`
  *also* contains `Unexpected field`/`is not allowed`. *Evidence (mutation M5):*
  removed `"[cluster_config]"` from `tarantoolConfigErrorSignatures` — **suite
  still passed**, because the other signature still matched. The
  `[cluster_config]`-only path has no test that depends on it.
- **No false-positive guard.** There is no negative case for a *non-config* crash
  line that happens to contain `Unexpected field` or `is not allowed` (substring
  match is case-sensitive but otherwise loose). The detector runs on any crashed
  tarantool container's log tail, so a non-config fatal line with those words
  would be mislabeled `ConfigRejected`, and no test would notice.
*Fixed:* added two `TestExtractConfigRejection` cases — `"cluster_config tag
only"` (matches solely via `[cluster_config]`, so dropping that signature now
fails) and `"fatal but not config"` (`F> can't bind …`, which has `F>` but not
`config`, so it must NOT match — guards the AND, not OR). Verified: removing the
`[cluster_config]` signature, and flipping the `F> && config` AND to OR, each now
fail the test.

### P5 — The probe regex is checked for *presence*, not *behavior*  ·  LOW  ·  FIXED

`probe_test.go` (`TestReadinessProbeAnchoredMatch`) asserts the command string
contains the anchored pattern `^- *running$` and does not contain a loose
`grep -q running`. It never runs the matcher against sample `box.info.status`
outputs, so a subtly-wrong-but-still-anchored regex (e.g. dropping the ` *`) would
pass. Low severity because the anchoring is the actual RS3 regression guard.
*Fixed:* added `TestReadinessProbeRegexBehavior` (`probe_test.go`), which extracts
the ERE *from the real probe command* (no drift) and runs it over sample lines:
`- running`/`-  running` accept, `- loading`/`running`/`  - running`/`- running
now` reject. Verified: replacing the anchored regex with a loose `running` now
fails.

### P6 — envtest tests have no isolation or cleanup  ·  LOW (latent)

All integration tests share `namespace: default`, never delete their objects, and
do not use `t.Parallel` (verified: the only `t.Parallel` is in a pure unit test,
`resources_test.go`). They pass today only because (a) they run sequentially and
(b) every test uses a **distinct** object name (verified: no name is reused across
`reconcile_test.go`/`edges_test.go`). This is fragile: adding `t.Parallel`, or
reusing a cluster name, would cause cross-test contamination via the controllers'
namespace-wide `List`/watches — a future flake, not a current one.
*Fix:* a per-test namespace (or `t.Cleanup` deleting created objects); then
parallelism and name reuse become safe.

---

## End-to-end (kind) tests

The 23 `test/e2e/kind-*.sh` scripts are, as *scripts*, well built: all use
`set -euo pipefail`, all have a `fail()` helper, and — checked one by one — every
readiness poll loop is followed by a hard `… || fail "…"` re-assertion, so a
**timeout falls through to a failure, never a silent pass** (the most common shell
e2e bug; absent here). All 23 have a Makefile target. This session I ran
`kind-role-rpm.sh` and `kind-role-rpm-pvc.sh` end-to-end (pass) and a
`reload.yaml`-equivalent 3-instance cluster (formed 3/3, hot-reloaded). I did
**not** re-run the other scenarios — in this sandbox kind nodes can't pull
`tarantool/tarantool:3`, which the committed scripts assume.

### E1 — Half the e2e scenarios never run in CI  ·  HIGH

`.github/workflows/e2e.yml` runs a 9-scenario matrix; `helm.yml`/`samples.yml`
add two more. But **12 of the 23 Makefile e2e targets are in no workflow at all**
(verified by diffing `make` targets against every file in `.github/workflows/`):

```
test-e2e-leader       leader-aware rollout (followers-first, leader-last)
test-e2e-reload       config hot-reload (no-restart)
test-e2e-rolling      rolling image update under load
test-e2e-upgrade      box.schema.upgrade() after image roll
test-e2e-nodeloss     dead-node remediation (multi-node)
test-e2e-rebalance    vshard auto-bootstrap + bucket rebalance
test-e2e-degraded     Degraded phase on a stuck instance
test-e2e-coexist      Cartridge + T3 operator coexistence
test-e2e-load         data integrity under load
test-e2e-roles-dynamic  add a role at runtime, no restart   (added this session)
test-e2e-role-rpm       RPM role baked into the image       (added this session)
test-e2e-role-rpm-pvc   RPM role delivered via a PVC        (added this session)
```

These cover the operator's **most complex behaviors** (leadership orchestration,
hot reload, remediation, rebalance, upgrade). Their only validation is a human
typing `make test-e2e-X`; a regression in any of them keeps CI green. Three of
them are e2e I added this session and forgot to wire into the matrix — so my own
new tests do not run automatically.

*Why some are excluded is partly legitimate:* `nodeloss` needs a multi-node kind
cluster; `rebalance` needs the vshard image; `upgrade` needs two tarantool
versions; the `role-rpm` pair needs network (apt) at image-build. But "harder to
run" became "silently not run."
*Fixed:* all 12 were added to the `e2e.yml` matrix (now 21 scenarios; `helm` and
`samples` keep their own workflows). Each script is self-contained — it creates
its own kind cluster (multi-node for `nodeloss`) and builds whatever it needs
(`tarantool-vshard`, `cartridge-kv`, the operator image) — so they run as plain
`make <target>` jobs like the rest. `timeout-minutes` raised 25 → 30 for the
heavier ones (rolling/upgrade pull two tarantool versions; `role-rpm*` build
images). `fail-fast: false` is kept so one failure doesn't mask the others.
First CI run will exercise these for the first time and may surface
environment-specific issues — which is exactly the point of un-dormanting them.

> **Since superseded:** the RPM-delivery scenarios (`role-rpm`, `role-rpm-pvc`)
> and the raw `roles-dynamic` test were later removed — role delivery is now the
> one-command `./deliver-role` (a ConfigMap on `LUA_PATH`), covered by
> `test-e2e-deliver-role`. The matrix is 19 scenarios.

### E2 — Stale header comment in `e2e.yml`  ·  LOW  ·  FIXED

`e2e.yml`'s comment claimed each scenario "runs the operator out-of-cluster" and
that "none use sharding.roles," both false (persistence/leader/reload/rolling run
in-cluster; rebalance uses vshard). Rewritten to describe the in/out-of-cluster
split and the scenarios that build extra images / need multi-node or pinned
versions.

## Hypotheses that mutation testing DEBUNKED (the suite is solid here)

Being agnostic cuts both ways. These looked like gaps on a read-through, but the
mutation was **caught** — do not "fix" these:

| Mutation introduced | Caught by |
|---|---|
| M1 `vshardbootstrap`: `case out == "ok"` → `"zzz"` (never matches) | `TestMaybeBootstrapVshard` (sticky-flag + event assertions) |
| M2 `replicaset_controller`: Ready phase → `Configuring` (the "envtest has no kubelet so Ready is never tested" claim) | `TestPhaseLifecycle` (fake-client, drives the real Ready path) |
| M3 `remediation`: quorum gate `ready < quorum` → `ready < 0` (gate removed) | `TestRemediateStuckPods` (quorum-not-held case) |
| M4 `rollout`: drop the "all instances ready" gate | `TestOrchestrateRollout` |
| M6 `render`: invert the `injectPasswords` `has`-check (precedence) | `TestInjectPasswordsEdgeCases` |
| M7 `rollout`: delete the **leader first** instead of last | `TestOrchestrateRollout` |
| M9 `delivered`: stop stripping `iproto.advertise` from the rollout hash | `TestRolloutHashIgnoresInstanceCount` |

The leader-aware rollout ordering, the remediation safety gates, the scale/rollout
hash stripping, the password precedence, and the phase lifecycle are all genuinely
exercised. The three external `Agent`-assisted audits flagged most of these as
"escapes"; mutation testing showed those claims were **false**. Lesson: trust the
mutation, not the assertion-count.

---

## Summary

| # | Problem | Severity | Evidence |
|---|---------|----------|----------|
| **E1** | **half the e2e scenarios ran in no CI workflow** — leader, reload, rolling, upgrade, nodeloss, rebalance, degraded, coexist, load — **FIXED** (added to `e2e.yml`; matrix now 19, incl. `deliver-role`) | HIGH | diff of Makefile targets vs `.github/workflows/` |
| P2 | probe numeric params unasserted — FIXED | mutation M8 escaped |
| P3 | idempotency test doesn't exercise the hash gate — FIXED (memtx-shrink test) | mutation M10 escaped |
| P4 | `configerror` signatures under-tested — FIXED | mutation M5 escaped + read |
| P1 | bare `go test` skips envtest (CI OK) — softened: loud skip banner | LOW–MED | skip→ok locally; `test.yml` provisions assets |
| P5 | probe regex tested for presence, not behavior — FIXED | read |
| P6 | envtest tests: no namespace isolation / cleanup | LOW (latent) | read + name-uniqueness check |
| E2 | `e2e.yml` header comment stale ("out-of-cluster") | LOW | read |

None of these is a correctness bug in the operator — they are places where a
*future* regression could slip past the tests. **E1 is the one that matters
most**: the operator's hardest behaviors have e2e coverage that CI never exercises,
so it can rot unnoticed. (P1, which I first rated HIGH, turned out to be CI-safe —
`test.yml` provisions envtest — and is only a local footgun.)

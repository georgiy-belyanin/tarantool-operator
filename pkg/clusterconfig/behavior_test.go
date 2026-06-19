package clusterconfig_test

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// renderHash returns just the canonical config hash for in.
func renderHash(in clusterconfig.Input) (string, error) {
	_, h, err := clusterconfig.RenderAndHash(in)
	return h, err
}

func baseInput() clusterconfig.Input {
	return clusterconfig.Input{
		Cluster: &v2alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default"},
			Spec: v2alpha1.ClusterSpec{
				Config: jsonExt(`{"replication":{"failover":"election"}}`),
			},
		},
		ReplicaSets: []v2alpha1.ReplicaSet{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "storage-001", Namespace: "default"},
				Spec:       v2alpha1.ReplicaSetSpec{ClusterName: "example", Group: "storages", Replicas: ptr(int32(2))},
			},
		},
	}
}

func TestHashStableAndSensitive(t *testing.T) {
	in := baseInput()

	h1, err := renderHash(in)
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	h2, err := renderHash(in)
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	if h1 != h2 {
		t.Errorf("hash not stable for equal input: %s != %s", h1, h2)
	}

	// Any rendered change must change the hash (it is the rollout trigger).
	cases := map[string]func(*clusterconfig.Input){
		"failover": func(i *clusterconfig.Input) { i.Cluster.Spec.Config = jsonExt(`{"replication":{"failover":"off"}}`) },
		"replicas": func(i *clusterconfig.Input) { i.ReplicaSets[0].Spec.Replicas = ptr(int32(3)) },
		"listenPort": func(i *clusterconfig.Input) {
			i.ReplicaSets[0].Spec.Config = jsonExt(`{"iproto":{"listen":[{"uri":"0.0.0.0:3302"}]}}`)
		},
		"passthrough": func(i *clusterconfig.Input) {
			i.Cluster.Spec.Config = jsonExt(`{"log":{"level":7}}`)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			mutated := baseInput()
			mutate(&mutated)
			h, err := renderHash(mutated)
			if err != nil {
				t.Fatalf("Hash() error = %v", err)
			}
			if h == h1 {
				t.Errorf("hash unchanged after mutating %q", name)
			}
		})
	}
}

func TestRenderPassthroughCannotOverrideOperatorKeys(t *testing.T) {
	in := baseInput()
	// Try to override an operator-owned key (the console socket the readiness
	// probe depends on) and add a genuinely new key (memtx.memory).
	in.Cluster.Spec.Config = jsonExt(`{"console":{"socket":"/tmp/hacked.sock"},"memtx":{"memory":123456}}`)

	out, err := clusterconfig.Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	tree, _ := parseYAML(t, out).(map[string]any)

	console, _ := tree["console"].(map[string]any)
	if got := console["socket"]; got != clusterconfig.AdminSocketPath {
		t.Errorf("operator-owned console.socket = %v, want %s (passthrough must not override)", got, clusterconfig.AdminSocketPath)
	}
	memtx, _ := tree["memtx"].(map[string]any)
	if memtx["memory"] == nil {
		t.Errorf("new passthrough key memtx.memory was dropped: %v", tree["memtx"])
	}
}

func TestRenderCredentialsWithoutPassword(t *testing.T) {
	in := baseInput()
	in.Cluster.Spec.Config = jsonExt(`{"replication":{"failover":"election"},"credentials":{"users":{"replicator":{"roles":["replication"]}}}}`) // not in Passwords

	out, err := clusterconfig.Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	tree, _ := parseYAML(t, out).(map[string]any)
	creds, _ := tree["credentials"].(map[string]any)
	users, _ := creds["users"].(map[string]any)
	replicator, _ := users["replicator"].(map[string]any)
	if replicator == nil {
		t.Fatalf("replicator user missing from credentials: %v", creds)
	}
	if _, hasPassword := replicator["password"]; hasPassword {
		t.Errorf("user without a resolved password must not render a password key: %v", replicator)
	}
	if replicator["roles"] == nil {
		t.Errorf("user roles should still render: %v", replicator)
	}
}

// TestInjectPasswordsEdgeCases covers the malformed/edge user-entry shapes the
// password injection must survive: a null user, a non-map user, an inline
// password (which wins over the Secret), and a user whose Secret entry is empty.
func TestInjectPasswordsEdgeCases(t *testing.T) {
	in := baseInput()
	in.Cluster.Spec.Config = jsonExt(`{"credentials":{"users":{
		"nulluser":   null,
		"garbage":    "not-a-map",
		"inline":     {"password":"from-cr","roles":["replication"]},
		"emptypw":    {"roles":["replication"]},
		"normal":     {"roles":["replication"]}
	}}}`)
	in.Passwords = map[string]string{
		"nulluser": "null-secret",
		"inline":   "from-secret",
		"emptypw":  "",
		"normal":   "normal-secret",
	}

	tree := renderTree(t, in)
	users := submap(t, submap(t, tree, "credentials"), "users")

	// A null user entry still receives its Secret password (the renderer
	// materializes the map for it).
	if u := submap(t, users, "nulluser"); u["password"] != "null-secret" {
		t.Errorf("nulluser password = %v, want null-secret", u["password"])
	}
	// A non-map user value passes through untouched (Tarantool will reject it —
	// not the operator's call) and must not panic the renderer.
	if users["garbage"] != "not-a-map" {
		t.Errorf("garbage user mangled: %v", users["garbage"])
	}
	// An inline password wins over the Secret.
	if u := submap(t, users, "inline"); u["password"] != "from-cr" {
		t.Errorf("inline password = %v, want from-cr (inline wins)", u["password"])
	}
	// An empty Secret value injects nothing.
	if u := submap(t, users, "emptypw"); u["password"] != nil {
		t.Errorf("emptypw must get no password, got %v", u["password"])
	}
	// The plain case still works.
	if u := submap(t, users, "normal"); u["password"] != "normal-secret" {
		t.Errorf("normal password = %v, want normal-secret", u["password"])
	}
}

// TestHashChangesOnPasswordRotation: rotating a credential password changes the
// rendered config Secret, so the hash (the rollout trigger) must change too.
func TestHashChangesOnPasswordRotation(t *testing.T) {
	in := baseInput()
	in.Cluster.Spec.Config = jsonExt(`{"replication":{"failover":"election"},"credentials":{"users":{"replicator":{"roles":["replication"]}}}}`)

	in.Passwords = map[string]string{"replicator": "old"}
	h1, err := renderHash(in)
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	in.Passwords = map[string]string{"replicator": "new"}
	h2, err := renderHash(in)
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	if h1 == h2 {
		t.Errorf("hash unchanged after password rotation")
	}
}

// TestReplicaSetScopePassthrough: a ReplicaSet's spec.config merges at the
// replicaset scope, but operator-managed keys still win and brand-new keys pass
// through.
func TestReplicaSetScopePassthrough(t *testing.T) {
	in := baseInput()
	in.ReplicaSets[0].Spec.Config = jsonExt(`{"bootstrap_leader":"wrong","memtx":{"memory":42}}`)

	rs := replicaSetTree(t, renderTree(t, in), "storages", "storage-001")
	if got := rs["bootstrap_leader"]; got != "storage-001-0" {
		t.Errorf("operator bootstrap_leader = %v, want storage-001-0 (passthrough must not override)", got)
	}
	if mem := submap(t, rs, "memtx")["memory"]; mem != float64(42) {
		t.Errorf("replicaset passthrough memtx.memory = %v, want 42", mem)
	}
}

// TestMergeUnderlayPreservesOperatorSubkeys: passthrough that targets a nested
// operator map (replication) must deep-merge — adding a new sub-key without
// clobbering the operator's existing sub-keys.
func TestMergeUnderlayPreservesOperatorSubkeys(t *testing.T) {
	in := baseInput() // failover: election
	in.ReplicaSets[0].Spec.Config = jsonExt(`{"replication":{"bootstrap_strategy":"auto","timeout":31}}`)

	rs := replicaSetTree(t, renderTree(t, in), "storages", "storage-001")
	repl := submap(t, rs, "replication")
	if repl["bootstrap_strategy"] != "config" {
		t.Errorf("operator replication.bootstrap_strategy = %v, want config (not overridden)", repl["bootstrap_strategy"])
	}
	if repl["timeout"] != float64(31) {
		t.Errorf("new replication.timeout = %v, want 31 (deep-merged sibling)", repl["timeout"])
	}
}

// --- problematic behaviors documented by the review (REVIEW-1.md) ------------
//
// The tests below LOCK IN behavior the review flags as wrong. They assert the
// current (problematic) output so the regression is visible; when the behavior
// is fixed, these tests must be updated. Each references its review finding.

// TestMultiInstanceOffHasNoWritableLeader documents REVIEW-1 R2: with the
// default failover mode "off" and more than one instance, the renderer emits
// neither a replicaset `leader` nor any instance `database.mode`, so no instance
// is declared writable. SHOULD CHANGE: for multi-instance `off` the operator
// must declare a writable instance (database.mode/leader) or reject the config.
// TestMultiInstanceOffDeclaresWritable verifies the RP3/RP6 fix: in off mode a
// replica set has no leader key (off mode has none), but the first instance is
// made writable via database.mode: rw so the set can bootstrap — without it,
// every instance is read-only and startup fails with "No leader to register new
// instance" (verified against tarantool 3.7). Exactly one instance is rw.
func TestMultiInstanceOffDeclaresWritable(t *testing.T) {
	in := baseInput()
	in.Cluster.Spec.Config = jsonExt(`{"replication":{"failover":"off"}}`)
	in.ReplicaSets[0].Spec.Replicas = ptr(int32(3))

	rs := replicaSetTree(t, renderTree(t, in), "storages", "storage-001")

	if _, ok := rs["leader"]; ok {
		t.Errorf("off mode must not set a replicaset leader (it uses database.mode): %v", rs["leader"])
	}
	instances := submap(t, rs, "instances")
	rwCount := 0
	for name, v := range instances {
		inst, _ := v.(map[string]any)
		db, _ := inst["database"].(map[string]any)
		if db != nil && db["mode"] == "rw" {
			rwCount++
			if name != "storage-001-0" {
				t.Errorf("writable instance = %q, want storage-001-0 (the first instance)", name)
			}
		}
	}
	if rwCount != 1 {
		t.Errorf("off-mode replica set must declare exactly one writable instance, got %d", rwCount)
	}
}

// TestBootstrapLeaderEmittedUnconditionally documents REVIEW-1 R13: a
// single-instance, failover:off replicaset still gets bootstrap_leader +
// bootstrap_strategy:config even though neither is needed there. SHOULD CHANGE:
// gate this on replicas>1 (and prefer bootstrap_strategy:native on Tarantool
// >= 3.4 — see review suggestion S1), so single instances stay clean.
func TestBootstrapLeaderEmittedUnconditionally(t *testing.T) {
	in := baseInput()
	in.Cluster.Spec.Config = jsonExt(`{"replication":{"failover":"off"}}`)
	in.ReplicaSets[0].Spec.Replicas = ptr(int32(1))

	rs := replicaSetTree(t, renderTree(t, in), "storages", "storage-001")
	if rs["bootstrap_leader"] != "storage-001-0" {
		t.Errorf("R13 regression check: bootstrap_leader no longer emitted for a single instance — behavior changed, update this test")
	}
	if submap(t, rs, "replication")["bootstrap_strategy"] != "config" {
		t.Errorf("R13 regression check: bootstrap_strategy no longer 'config' — behavior changed, update this test (consider 'native', see S1)")
	}
}

// TestRenderAndHashConsistent: RenderAndHash (single tree build) must agree with
// Render and Hash (separate builds) — same hash, and YAML parsing to the same tree.
func TestRenderAndHashConsistent(t *testing.T) {
	in := baseInput()
	in.ReplicaSets[0].Status.Bootstrapped = true // exercise the autoexpel branch too

	rendered, hash, err := clusterconfig.RenderAndHash(in)
	if err != nil {
		t.Fatalf("RenderAndHash() error = %v", err)
	}
	wantHash, err := renderHash(in)
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	if hash != wantHash {
		t.Errorf("RenderAndHash hash = %s, want %s (== Hash)", hash, wantHash)
	}
	sep, err := clusterconfig.Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	// YAML key order isn't byte-stable; compare parsed structures.
	if a, b := parseYAML(t, rendered), parseYAML(t, sep); !reflect.DeepEqual(a, b) {
		t.Errorf("RenderAndHash YAML differs semantically from Render")
	}
}

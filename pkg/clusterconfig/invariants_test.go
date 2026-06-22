package clusterconfig_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// --- small helpers for navigating the parsed config tree ---------------------

func renderTree(t *testing.T, in clusterconfig.Input) map[string]any {
	t.Helper()
	out, err := clusterconfig.Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	tree, ok := parseYAML(t, out).(map[string]any)
	if !ok {
		t.Fatalf("rendered config is not a mapping")
	}
	return tree
}

// submap returns m[key] as a map, failing the test if it is missing or not a map.
func submap(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("key %q missing", key)
	}
	sub, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("key %q is %T, want map", key, v)
	}
	return sub
}

// replicaSetTree digs to groups.<group>.replicasets.<rs>.
func replicaSetTree(t *testing.T, tree map[string]any, group, rs string) map[string]any {
	t.Helper()
	return submap(t, submap(t, submap(t, submap(t, tree, "groups"), group), "replicasets"), rs)
}

// --- bootstrap_leader scope: the crux validated against Tarantool 3.7 --------

// TestBootstrapLeaderScope locks in the deterministic-bootstrap fix (commit
// b0d2228): every replicaset must carry bootstrap_leader as a *sibling* of
// replication (not nested under it — Tarantool 3.7 rejects
// replication.bootstrap_leader with `Unexpected field`), and
// replication.bootstrap_strategy must be "config".
func TestBootstrapLeaderScope(t *testing.T) {
	tree := renderTree(t, baseInput())
	rs := replicaSetTree(t, tree, "storages", "storage-001")

	if got := rs["bootstrap_leader"]; got != "storage-001-0" {
		t.Errorf("bootstrap_leader = %v, want storage-001-0 (replicaset-scope sibling)", got)
	}
	repl := submap(t, rs, "replication")
	if got := repl["bootstrap_strategy"]; got != "config" {
		t.Errorf("replication.bootstrap_strategy = %v, want config", got)
	}
	if _, nested := repl["bootstrap_leader"]; nested {
		t.Errorf("bootstrap_leader must NOT be nested under replication (Tarantool 3.7 rejects it there)")
	}
}

// --- manual leader is mode-gated ---------------------------------------------

func TestLeaderOnlyInManualMode(t *testing.T) {
	withLeader := func(failover, leaderJSON string) map[string]any {
		in := baseInput()
		in.Cluster.Spec.Config = jsonExt(`{"replication":{"failover":"` + failover + `"}}`)
		if leaderJSON != "" {
			in.ReplicaSets[0].Spec.Config = jsonExt(`{"leader":"` + leaderJSON + `"}`)
		}
		return replicaSetTree(t, renderTree(t, in), "storages", "storage-001")
	}

	t.Run("manual with a user leader keeps it", func(t *testing.T) {
		rs := withLeader("manual", "storage-001-1")
		if got := rs["leader"]; got != "storage-001-1" {
			t.Errorf("leader = %v, want storage-001-1", got)
		}
	})
	t.Run("manual without a leader defaults to the first instance", func(t *testing.T) {
		// A manual replica set must name a leader, or every instance is read-only
		// and bootstrap fails with "No leader to register new instance".
		rs := withLeader("manual", "")
		if got := rs["leader"]; got != "storage-001-0" {
			t.Errorf("leader = %v, want storage-001-0 (operator default)", got)
		}
	})
	for _, mode := range []string{"election", "off", "supervised"} {
		t.Run("no operator leader default in "+mode, func(t *testing.T) {
			rs := withLeader(mode, "")
			if _, ok := rs["leader"]; ok {
				t.Errorf("operator must not add a leader in %q mode", mode)
			}
		})
	}
}

// --- instance listen / advertise URI -----------------------------------------

func TestInstanceListenAndAdvertise(t *testing.T) {
	in := baseInput()
	in.Cluster.Spec.Domain = "k8s.example"
	in.Cluster.Spec.Config = jsonExt(`{"iproto":{"listen":[{"uri":"0.0.0.0:4301"}]}}`)
	in.Cluster.Namespace = "tnt"
	in.ReplicaSets[0].Namespace = "tnt"

	rs := replicaSetTree(t, renderTree(t, in), "storages", "storage-001")
	instances := submap(t, rs, "instances")

	if len(instances) != 2 {
		t.Fatalf("want 2 instances, got %d: %v", len(instances), instances)
	}
	for _, name := range []string{"storage-001-0", "storage-001-1"} {
		inst, ok := instances[name].(map[string]any)
		if !ok {
			t.Fatalf("instance %q missing", name)
		}
		iproto := submap(t, inst, "iproto")

		// The user configured iproto.listen (global scope), so the operator must
		// not generate an instance-scope listen that would shadow it.
		if _, ok := iproto["listen"]; ok {
			t.Errorf("instance %q: operator must not generate listen when the user set one: %v", name, iproto["listen"])
		}

		peer := submap(t, submap(t, iproto, "advertise"), "peer")
		want := name + ".example.tnt.svc.k8s.example:4301"
		if peer["uri"] != want {
			t.Errorf("instance %q advertise.peer.uri = %v, want %s", name, peer["uri"], want)
		}
	}
}

// --- sharding wiring ----------------------------------------------------------

func TestShardingWiring(t *testing.T) {
	in := baseInput()
	in.Cluster.Spec.Config = jsonExt(`{"replication":{"failover":"election"},"credentials":{"users":{"storage":{"roles":["sharding"]}}}}`)
	in.ReplicaSets[0].Spec.Config = jsonExt(`{"sharding":{"roles":["storage"],"weight":2}}`)

	tree := renderTree(t, in)
	// Sharding user wired into iproto.advertise.sharding.login.
	shardingAdv := submap(t, submap(t, submap(t, tree, "iproto"), "advertise"), "sharding")
	if shardingAdv["login"] != "storage" {
		t.Errorf("iproto.advertise.sharding.login = %v, want storage", shardingAdv["login"])
	}
	// Per-replicaset roles + weight.
	rs := replicaSetTree(t, tree, "storages", "storage-001")
	sh := submap(t, rs, "sharding")
	if roles, _ := sh["roles"].([]any); len(roles) != 1 || roles[0] != "storage" {
		t.Errorf("replicaset sharding.roles = %v, want [storage]", sh["roles"])
	}
	if sh["weight"] != float64(2) {
		t.Errorf("replicaset sharding.weight = %v, want 2", sh["weight"])
	}

	// Every instance must advertise a client-connectable sharding URI: the global
	// advertise.sharding.login stops the inheritance from advertise.peer, and
	// vshard cannot use the 0.0.0.0 listen URI ("INADDR_ANY cannot be used to
	// create a client socket" — instance exits).
	inst := submap(t, submap(t, rs, "instances"), "storage-001-0")
	shAdv := submap(t, submap(t, submap(t, inst, "iproto"), "advertise"), "sharding")
	peerAdv := submap(t, submap(t, submap(t, inst, "iproto"), "advertise"), "peer")
	if shAdv["uri"] == nil || shAdv["uri"] != peerAdv["uri"] {
		t.Errorf("instance advertise.sharding.uri = %v, want the peer URI %v", shAdv["uri"], peerAdv["uri"])
	}
}

func TestUnshardedOmitsSharding(t *testing.T) {
	tree := renderTree(t, baseInput()) // baseInput has no Sharding
	if _, ok := tree["sharding"]; ok {
		t.Errorf("unsharded cluster must not render a global sharding section")
	}
	if iproto, ok := tree["iproto"].(map[string]any); ok {
		if adv, ok := iproto["advertise"].(map[string]any); ok {
			if _, ok := adv["sharding"]; ok {
				t.Errorf("unsharded cluster must not render iproto.advertise.sharding")
			}
		}
	}
	inst := submap(t, submap(t, replicaSetTree(t, tree, "storages", "storage-001"), "instances"), "storage-001-0")
	if adv := submap(t, submap(t, inst, "iproto"), "advertise"); adv["sharding"] != nil {
		t.Errorf("unsharded instance must not advertise a sharding URI: %v", adv["sharding"])
	}
}

// --- console is always enabled for the readiness probe -----------------------

func TestConsoleAlwaysEnabled(t *testing.T) {
	console := submap(t, renderTree(t, baseInput()), "console")
	if console["enabled"] != true {
		t.Errorf("console.enabled = %v, want true", console["enabled"])
	}
	if console["socket"] != clusterconfig.AdminSocketPath {
		t.Errorf("console.socket = %v, want %s", console["socket"], clusterconfig.AdminSocketPath)
	}
}

// --- grouping & cluster scoping ----------------------------------------------

func TestForeignReplicaSetsIgnored(t *testing.T) {
	in := baseInput()
	in.ReplicaSets = append(in.ReplicaSets, v2alpha1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "other-rs", Namespace: "default"},
		Spec:       v2alpha1.ReplicaSetSpec{ClusterName: "some-other-cluster", Group: "storages"},
	})

	rsets := submap(t, submap(t, submap(t, renderTree(t, in), "groups"), "storages"), "replicasets")
	if _, ok := rsets["other-rs"]; ok {
		t.Errorf("replicaset belonging to another cluster must be ignored: %v", rsets)
	}
	if _, ok := rsets["storage-001"]; !ok {
		t.Errorf("own replicaset missing: %v", rsets)
	}
}

func TestGroupDefaulting(t *testing.T) {
	in := baseInput()
	in.ReplicaSets[0].Spec.Group = "" // -> "default"

	groups := submap(t, renderTree(t, in), "groups")
	if _, ok := groups[v2alpha1.DefaultGroup]; !ok {
		t.Errorf("replicaset with empty group should land under %q: %v", v2alpha1.DefaultGroup, groups)
	}
}

// --- replication.timeout passthrough -----------------------------------------

func TestReplicationTimeoutRendered(t *testing.T) {
	in := baseInput()
	in.Cluster.Spec.Config = jsonExt(`{"replication":{"failover":"election","timeout":7}}`)

	repl := submap(t, renderTree(t, in), "replication")
	if repl["timeout"] != float64(7) {
		t.Errorf("replication.timeout = %v, want 7", repl["timeout"])
	}
}

// --- roles / roles_cfg --------------------------------------------------------

func TestRolesAndRolesCfg(t *testing.T) {
	in := baseInput()
	in.ReplicaSets[0].Spec.Config = jsonExt(`{"roles":["roles.crud-router","app.greeter"],"roles_cfg":{"app.greeter":{"greeting":"hi"}}}`)

	rs := replicaSetTree(t, renderTree(t, in), "storages", "storage-001")
	roles, _ := rs["roles"].([]any)
	if len(roles) != 2 || roles[0] != "roles.crud-router" {
		t.Errorf("roles = %v, want [roles.crud-router app.greeter]", rs["roles"])
	}
	greeter := submap(t, submap(t, rs, "roles_cfg"), "app.greeter")
	if greeter["greeting"] != "hi" {
		t.Errorf("roles_cfg.app.greeter.greeting = %v, want hi", greeter["greeting"])
	}
}

// --- credentials password is rendered when resolved --------------------------

func TestCredentialsPasswordRendered(t *testing.T) {
	in := baseInput()
	in.Cluster.Spec.Config = jsonExt(`{"replication":{"failover":"election"},"credentials":{"users":{"replicator":{"roles":["replication"]}}}}`)
	in.Passwords = map[string]string{"replicator": "s3cr3t"}

	tree := renderTree(t, in)
	user := submap(t, submap(t, submap(t, tree, "credentials"), "users"), "replicator")
	if user["password"] != "s3cr3t" {
		t.Errorf("password = %v, want s3cr3t", user["password"])
	}
	// The replication user is also wired as the peer advertise login.
	peer := submap(t, submap(t, submap(t, tree, "iproto"), "advertise"), "peer")
	if peer["login"] != "replicator" {
		t.Errorf("iproto.advertise.peer.login = %v, want replicator", peer["login"])
	}
}

// --- Hash is independent of ReplicaSet slice order ---------------------------

// TestHashIndependentOfReplicaSetOrder guards the rollout trigger against
// client.List returning ReplicaSets in a nondeterministic order: the hash must
// depend only on the set of replicasets, not their ordering, or pods would roll
// spuriously whenever the List order changed.
func TestHashIndependentOfReplicaSetOrder(t *testing.T) {
	mk := func(name, group string) v2alpha1.ReplicaSet {
		return v2alpha1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       v2alpha1.ReplicaSetSpec{ClusterName: "example", Group: group, Replicas: ptr(int32(1))},
		}
	}
	base := baseInput()
	a := mk("storage-001", "storages")
	b := mk("storage-002", "storages")
	c := mk("router-001", "routers")

	in1 := base
	in1.ReplicaSets = []v2alpha1.ReplicaSet{a, b, c}
	in2 := base
	in2.ReplicaSets = []v2alpha1.ReplicaSet{c, a, b}

	h1, err := renderHash(in1)
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	h2, err := renderHash(in2)
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	if h1 != h2 {
		t.Errorf("hash depends on replicaset slice order: %s != %s", h1, h2)
	}
}

// --- nil cluster error paths -------------------------------------------------

func TestNilClusterErrors(t *testing.T) {
	if _, err := clusterconfig.Render(clusterconfig.Input{}); err == nil {
		t.Errorf("Render(nil cluster) should error")
	}
	if _, err := renderHash(clusterconfig.Input{}); err == nil {
		t.Errorf("Hash(nil cluster) should error")
	}
}

// TestStorageWeightZeroedOnDeletion: a storage replica set with a deletion
// timestamp renders with sharding.weight 0, so vshard drains it before the
// operator's finalizer lets the delete proceed. A router (no storage role) is
// untouched even while deleting.
func TestStorageWeightZeroedOnDeletion(t *testing.T) {
	deleting := metav1.NewTime(metav1.Now().Time)

	t.Run("deleting storage -> weight 0", func(t *testing.T) {
		in := baseInput()
		in.ReplicaSets[0].Spec.Config = jsonExt(`{"sharding":{"roles":["storage"],"weight":3}}`)
		in.ReplicaSets[0].DeletionTimestamp = &deleting
		rs := replicaSetTree(t, renderTree(t, in), "storages", "storage-001")
		if w := submap(t, rs, "sharding")["weight"]; w != float64(0) {
			t.Errorf("deleting storage sharding.weight = %v, want 0", w)
		}
	})

	t.Run("live storage keeps its weight", func(t *testing.T) {
		in := baseInput()
		in.ReplicaSets[0].Spec.Config = jsonExt(`{"sharding":{"roles":["storage"],"weight":3}}`)
		rs := replicaSetTree(t, renderTree(t, in), "storages", "storage-001")
		if w := submap(t, rs, "sharding")["weight"]; w != float64(3) {
			t.Errorf("live storage sharding.weight = %v, want 3", w)
		}
	})

	t.Run("deleting non-storage (router) untouched", func(t *testing.T) {
		in := baseInput()
		in.ReplicaSets[0].Spec.Config = jsonExt(`{"sharding":{"roles":["router"]}}`)
		in.ReplicaSets[0].DeletionTimestamp = &deleting
		rs := replicaSetTree(t, renderTree(t, in), "storages", "storage-001")
		if _, ok := submap(t, rs, "sharding")["weight"]; ok {
			t.Errorf("router must not gain a weight key on deletion")
		}
	})
}

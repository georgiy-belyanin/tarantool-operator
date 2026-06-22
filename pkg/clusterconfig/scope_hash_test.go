package clusterconfig_test

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// delivered renders in and parses the result.
func delivered(t *testing.T, in clusterconfig.Input) *clusterconfig.Delivered {
	t.Helper()
	cfg, err := clusterconfig.Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	d, err := clusterconfig.ParseDelivered(cfg)
	if err != nil {
		t.Fatalf("ParseDelivered() error = %v", err)
	}
	return d
}

// scopeHash renders in and returns the instance-scope hash for storages/storage-001.
func scopeHash(t *testing.T, in clusterconfig.Input) string {
	t.Helper()
	h, err := delivered(t, in).InstanceScopeHash("storages", "storage-001")
	if err != nil {
		t.Fatalf("InstanceScopeHash() error = %v", err)
	}
	return h
}

// TestInstanceScopeHash is the P3 regression test: a replica set's scope hash is
// stable when an *unrelated* replica set changes, but changes when the replica
// set's own subtree or the global scope changes — so the controller rolls only
// the replica sets whose own effective config changed, not the whole cluster.
func TestInstanceScopeHash(t *testing.T) {
	base := baseInput() // one replica set: storages/storage-001 (election)
	h0 := scopeHash(t, base)

	t.Run("adding another replica set does not change this one's hash", func(t *testing.T) {
		in := baseInput()
		in.ReplicaSets = append(in.ReplicaSets, v2alpha1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: "storage-002", Namespace: "default"},
			Spec:       v2alpha1.ReplicaSetSpec{ClusterName: "example", Group: "storages", Replicas: ptr(int32(2)), Config: jsonExt(`{"sharding":{"roles":["storage"]}}`)},
		})
		if h := scopeHash(t, in); h != h0 {
			t.Errorf("scope hash changed when an unrelated replica set was added (P3): %s != %s", h, h0)
		}
	})

	t.Run("changing this replica set's own config changes its hash", func(t *testing.T) {
		in := baseInput()
		in.ReplicaSets[0].Spec.Replicas = ptr(int32(5))
		if h := scopeHash(t, in); h == h0 {
			t.Errorf("scope hash unchanged after changing this replica set's replicas")
		}
	})

	t.Run("changing the global scope changes the hash", func(t *testing.T) {
		in := baseInput()
		in.ReplicaSets[0].Spec.Config = jsonExt(`{"iproto":{"listen":[{"uri":"0.0.0.0:3302"}]}}`)
		if h := scopeHash(t, in); h == h0 {
			t.Errorf("scope hash unchanged after changing a global (cluster-wide) setting")
		}
	})
}

// TestRolloutHashIgnoresRoles is the dynamic-role-delivery fix: enabling or
// reconfiguring a Tarantool 3 role (config.roles / config.roles_cfg) must NOT
// change the RolloutHash — roles apply/stop on config:reload(), so the operator
// hot-reloads them instead of rolling the pods — while the full InstanceScopeHash
// still changes (the no-super-user path cannot reload, so it must roll).
func TestRolloutHashIgnoresRoles(t *testing.T) {
	// Both inputs carry the same sharding role; they differ ONLY in config.roles /
	// config.roles_cfg, so any hash delta is attributable to the role change alone.
	without := baseInput()
	without.ReplicaSets[0].Spec.Config = jsonExt(`{"sharding":{"roles":["storage"]}}`)
	with := baseInput()
	with.ReplicaSets[0].Spec.Config = jsonExt(`{"sharding":{"roles":["storage"]},"roles":["greeter"],"roles_cfg":{"greeter":{"greeting":"hi"}}}`)

	d0, d1 := delivered(t, without), delivered(t, with)

	r0, err := d0.RolloutHash("storages", "storage-001")
	if err != nil {
		t.Fatal(err)
	}
	r1, err := d1.RolloutHash("storages", "storage-001")
	if err != nil {
		t.Fatal(err)
	}
	if r1 != r0 {
		t.Errorf("RolloutHash changed after enabling a role (%s != %s) — a role change would roll the pods instead of hot-reloading", r1, r0)
	}

	s0, _ := d0.InstanceScopeHash("storages", "storage-001")
	s1, _ := d1.InstanceScopeHash("storages", "storage-001")
	if s1 == s0 {
		t.Errorf("InstanceScopeHash unchanged after a roles change — the no-super-user path must roll to deliver it")
	}
}

// TestContainsReplicaSet guards the F1 ordering fix: a config rendered before the
// ReplicaSets are observed (no groups) must report the set absent, so the
// ReplicaSet reconciler waits instead of building a StatefulSet from a
// topology-less config (which would later roll the pods and split the bootstrap
// leader on ephemeral storage).
func TestContainsReplicaSet(t *testing.T) {
	full, err := clusterconfig.Render(baseInput())
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	df, err := clusterconfig.ParseDelivered(full)
	if err != nil {
		t.Fatalf("ParseDelivered() error = %v", err)
	}
	if !df.ContainsReplicaSet("storages", "storage-001") {
		t.Errorf("ContainsReplicaSet(full) = false, want true")
	}
	if df.ContainsReplicaSet("storages", "nope") {
		t.Errorf("ContainsReplicaSet(absent) = true, want false")
	}
	// A topology-less config (the nrs=0 first render) must report absent.
	dn, err := clusterconfig.ParseDelivered([]byte("replication:\n  failover: election\n"))
	if err != nil {
		t.Fatalf("ParseDelivered() error = %v", err)
	}
	if dn.ContainsReplicaSet("storages", "storage-001") {
		t.Errorf("ContainsReplicaSet(no-groups) = true, want false")
	}
}

// TestRolloutHashTracksInstanceConfigContent guards the membership exclusion from
// over-stripping: RolloutHash must still CHANGE when an instance's own config
// content (spec.instanceConfigs) changes — instance-scope static options cannot be
// applied by config:reload, so the only way to deliver them is a pod roll. (Before
// the fix the whole instances subtree was excluded, so such a change silently
// never applied.)
func TestRolloutHashTracksInstanceConfigContent(t *testing.T) {
	r0, err := delivered(t, baseInput()).RolloutHash("storages", "storage-001")
	if err != nil {
		t.Fatal(err)
	}

	in := baseInput()
	in.ReplicaSets[0].Spec.InstanceConfigs = map[string]apiextensionsv1.JSON{
		// memtx.allocator is a static (startup-only) option at the instance scope.
		"1": *jsonExt(`{"memtx":{"allocator":"small"}}`),
	}
	r1, err := delivered(t, in).RolloutHash("storages", "storage-001")
	if err != nil {
		t.Fatal(err)
	}
	if r1 == r0 {
		t.Errorf("RolloutHash unchanged after an instanceConfigs change — instance-scope static config would silently never apply")
	}
}

// TestRolloutHashIgnoresInstanceCount is the fix for "scale-up rolls the leader":
// changing the replica COUNT must not change the RolloutHash (so the StatefulSet
// adds/removes pods without rolling the survivors — topology is reload-applied),
// while the full InstanceScopeHash still changes (the no-super-user path must
// roll, since it cannot reload the new peer list).
func TestRolloutHashIgnoresInstanceCount(t *testing.T) {
	mk := func(replicas int32) clusterconfig.Input {
		in := baseInput()
		in.ReplicaSets[0].Spec.Replicas = ptr(replicas)
		return in
	}
	three := delivered(t, mk(3))
	five := delivered(t, mk(5))

	r3, err := three.RolloutHash("storages", "storage-001")
	if err != nil {
		t.Fatal(err)
	}
	r5, err := five.RolloutHash("storages", "storage-001")
	if err != nil {
		t.Fatal(err)
	}
	if r3 != r5 {
		t.Errorf("RolloutHash changed with the instance count (%s != %s) — scaling would roll the survivors", r3, r5)
	}

	s3, _ := three.InstanceScopeHash("storages", "storage-001")
	s5, _ := five.InstanceScopeHash("storages", "storage-001")
	if s3 == s5 {
		t.Errorf("InstanceScopeHash must change with the instance count (no-super-user path rolls to deliver the new peer list)")
	}

	// A non-topology change (global memtx.memory, non-dynamic-stripped only in the
	// rollout sense) still moves the RolloutHash — we did not over-strip.
	in := mk(3)
	in.Cluster.Spec.Config = jsonExt(`{"replication":{"failover":"election"},"sharding":{"bucket_count":1234}}`)
	rChanged, _ := delivered(t, in).RolloutHash("storages", "storage-001")
	if rChanged == r3 {
		t.Errorf("RolloutHash must still change on a non-topology config change")
	}
}

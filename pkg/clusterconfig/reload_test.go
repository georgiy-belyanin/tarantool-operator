package clusterconfig_test

import (
	"testing"

	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// TestConfigHashLabelRendered: the rendered config carries the content hash as the
// global operator_config_hash label, equal to Hash(in) — so an instance can report
// which config version it has loaded.
func TestConfigHashLabelRendered(t *testing.T) {
	in := baseInput()
	tree := renderTree(t, in)
	want, err := renderHash(in)
	if err != nil {
		t.Fatalf("Hash() error = %v", err)
	}
	labels := submap(t, tree, "labels")
	if labels[clusterconfig.ConfigHashLabel] != want {
		t.Errorf("labels.%s = %v, want %s (== Hash)", clusterconfig.ConfigHashLabel, labels[clusterconfig.ConfigHashLabel], want)
	}
}

// rolloutHash renders in and returns RolloutHash for storages/storage-001.
func rolloutHash(t *testing.T, in clusterconfig.Input) string {
	t.Helper()
	h, err := delivered(t, in).RolloutHash("storages", "storage-001")
	if err != nil {
		t.Fatalf("RolloutHash() error = %v", err)
	}
	return h
}

// TestRolloutHashExcludesDynamicKeys: changing a dynamic key (log.level) must NOT
// change the RolloutHash (so pods are not rolled — the operator reloads it), but it
// MUST change the full InstanceScopeHash. A non-dynamic change (listenPort) changes
// the RolloutHash.
func TestRolloutHashExcludesDynamicKeys(t *testing.T) {
	withLog := func(level string) clusterconfig.Input {
		in := baseInput()
		in.Cluster.Spec.Config = jsonExt(`{"log":{"level":` + level + `}}`)
		return in
	}

	if a, b := rolloutHash(t, withLog("5")), rolloutHash(t, withLog("7")); a != b {
		t.Errorf("RolloutHash changed on a dynamic log.level change: %s != %s", a, b)
	}
	// InstanceScopeHash (full) must reflect the change.
	if a, b := scopeHash(t, withLog("5")), scopeHash(t, withLog("7")); a == b {
		t.Errorf("InstanceScopeHash should change on a log.level change")
	}
	// A non-dynamic change still rolls.
	base := withLog("5")
	bumped := withLog("5")
	bumped.ReplicaSets[0].Spec.Config = jsonExt(`{"iproto":{"listen":[{"uri":"0.0.0.0:3302"}]}}`)
	if a, b := rolloutHash(t, base), rolloutHash(t, bumped); a == b {
		t.Errorf("RolloutHash should change on a non-dynamic listenPort change")
	}

	// The FIRST appearance of a section that only carries a dynamic key (no log
	// section at all -> log.level) must not change the hash either: stripping the
	// key has to prune the empty parent map it leaves behind, or that one change
	// rolls the pods despite being hot-reloadable (caught live in the kind e2e).
	withBaseAndLog := baseInput()
	withBaseAndLog.Cluster.Spec.Config = jsonExt(`{"replication":{"failover":"election"},"log":{"level":7}}`)
	if a, b := rolloutHash(t, baseInput()), rolloutHash(t, withBaseAndLog); a != b {
		t.Errorf("RolloutHash changed when a dynamic-only section first appeared: %s != %s", a, b)
	}
}

// TestMemtxMemory: extraction for the grow-only decrease warning (F31).
func TestMemtxMemory(t *testing.T) {
	in := baseInput()
	in.Cluster.Spec.Config = jsonExt(`{"memtx":{"memory":268435456}}`)
	out, err := clusterconfig.Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	d, err := clusterconfig.ParseDelivered(out)
	if err != nil {
		t.Fatalf("ParseDelivered() error = %v", err)
	}
	if v, ok := d.MemtxMemory(); !ok || v != 268435456 {
		t.Errorf("MemtxMemory = %d,%v; want 268435456,true", v, ok)
	}
	dn, err := clusterconfig.ParseDelivered([]byte("replication:\n  failover: off\n"))
	if err != nil {
		t.Fatalf("ParseDelivered() error = %v", err)
	}
	if _, ok := dn.MemtxMemory(); ok {
		t.Errorf("MemtxMemory should be ok=false when unset")
	}
}

package clusterconfig_test

import (
	"testing"
)

// TestReplicaSetAutoexpelWhenBootstrapped is the P4 behavior: once a replica set
// has bootstrapped, it renders replication.autoexpel (enabled, by: prefix, prefix =
// replicaset name) so an instance dropped from the config on scale-down is expelled
// from _cluster instead of lingering as an orphan in box.info.replication.
func TestReplicaSetAutoexpelWhenBootstrapped(t *testing.T) {
	in := baseInput()
	in.ReplicaSets[0].Status.Bootstrapped = true

	rs := replicaSetTree(t, renderTree(t, in), "storages", "storage-001")
	repl := submap(t, rs, "replication")

	ax, ok := repl["autoexpel"].(map[string]any)
	if !ok {
		t.Fatalf("replication.autoexpel missing or not a map: %v", repl["autoexpel"])
	}
	if ax["enabled"] != true {
		t.Errorf("autoexpel.enabled = %v, want true", ax["enabled"])
	}
	if ax["by"] != "prefix" {
		t.Errorf("autoexpel.by = %v, want prefix", ax["by"])
	}
	if ax["prefix"] != "storage-001" {
		t.Errorf("autoexpel.prefix = %v, want storage-001 (the replicaset name, matching <rs>-<ordinal> instances)", ax["prefix"])
	}
}

// TestReplicaSetNoAutoexpelBeforeBootstrap is the F1 fix: a fresh,
// not-yet-bootstrapped replica set must NOT render replication.autoexpel. At fresh
// concurrent bootstrap autoexpel races the join protocol and expels peers, so every
// instance ends up in its own replica set (split-brain, reproduced in the kind
// e2e). bootstrap_strategy stays "config" either way.
func TestReplicaSetNoAutoexpelBeforeBootstrap(t *testing.T) {
	in := baseInput() // Status.Bootstrapped defaults to false

	rs := replicaSetTree(t, renderTree(t, in), "storages", "storage-001")
	repl := submap(t, rs, "replication")

	if _, present := repl["autoexpel"]; present {
		t.Errorf("autoexpel must NOT be rendered before the set is bootstrapped: %v", repl["autoexpel"])
	}
	if repl["bootstrap_strategy"] != "config" {
		t.Errorf("bootstrap_strategy = %v, want config (always)", repl["bootstrap_strategy"])
	}
}

// TestAutoexpelExcludedFromRolloutHash: flipping autoexpel on (when the set becomes
// bootstrapped) must not change the per-replicaset rollout hash, so a freshly-formed
// set is not rolled just to enable autoexpel — it only matters for future
// scale-down, which restarts the pods anyway.
func TestAutoexpelExcludedFromRolloutHash(t *testing.T) {
	before := baseInput() // not bootstrapped
	after := baseInput()
	after.ReplicaSets[0].Status.Bootstrapped = true // autoexpel now rendered

	if h0, h1 := scopeHash(t, before), scopeHash(t, after); h0 != h1 {
		t.Errorf("rollout hash changed when autoexpel toggled on: %s != %s", h0, h1)
	}
}

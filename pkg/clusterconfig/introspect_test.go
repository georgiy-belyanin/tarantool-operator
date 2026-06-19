package clusterconfig_test

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

func introspect(t *testing.T, in clusterconfig.Input) clusterconfig.Settings {
	t.Helper()
	rendered, err := clusterconfig.Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	d, err := clusterconfig.ParseDelivered(rendered)
	if err != nil {
		t.Fatalf("ParseDelivered() error = %v", err)
	}
	return d.Settings("storages", "storage-001")
}

func TestIntrospectSettings(t *testing.T) {
	t.Run("reads failover, leader and port from the rendered config", func(t *testing.T) {
		in := baseInput()
		in.Cluster.Spec.Config = jsonExt(`{"replication":{"failover":"manual"}}`)
		in.ReplicaSets[0].Spec.Config = jsonExt(`{"leader":"storage-001-1","iproto":{"listen":[{"uri":"0.0.0.0:4301"}]}}`)

		s := introspect(t, in)
		if s.Failover != "manual" || s.Leader != "storage-001-1" || s.Port != 4301 {
			t.Errorf("Settings = %+v, want manual/storage-001-1/4301", s)
		}
	})

	t.Run("values set only via raw config sections are seen the same way", func(t *testing.T) {
		in := baseInput()
		in.Cluster.Spec.Config = jsonExt(`{"replication":{"failover":"election"}}`)

		s := introspect(t, in)
		if s.Failover != "election" {
			t.Errorf("Failover = %q, want election (read from the delivered config)", s.Failover)
		}
	})

	t.Run("built-in operator user is the default super user", func(t *testing.T) {
		s := introspect(t, baseInput())
		if s.SuperUser != clusterconfig.OperatorUser {
			t.Errorf("SuperUser = %q, want %q (the built-in user)", s.SuperUser, clusterconfig.OperatorUser)
		}
	})

	t.Run("super user comes from global credentials when the built-in user is disabled", func(t *testing.T) {
		in := baseInput()
		in.Cluster.Spec.DisableOperatorUser = true
		in.Cluster.Spec.Config = jsonExt(`{"credentials":{"users":{"replicator":{"roles":["replication"]},"admin":{"roles":["super"]}}}}`)

		s := introspect(t, in)
		if s.SuperUser != "admin" {
			t.Errorf("SuperUser = %q, want admin", s.SuperUser)
		}
	})

	t.Run("super user choice is deterministic with several candidates", func(t *testing.T) {
		// Two users carry super: the operator must always pick the same one
		// (first in name order), not whichever a map iteration yields — its
		// identity must not flap between reconciles.
		in := baseInput()
		in.Cluster.Spec.DisableOperatorUser = true
		in.Cluster.Spec.Config = jsonExt(`{"credentials":{"users":{"zadmin":{"roles":["super"]},"admin":{"roles":["super"]},"madmin":{"roles":["super"]}}}}`)

		for i := 0; i < 10; i++ {
			if s := introspect(t, in); s.SuperUser != "admin" {
				t.Fatalf("SuperUser = %q on attempt %d, want admin (first in name order)", s.SuperUser, i)
			}
		}
	})

	t.Run("no super role means no super user (built-in disabled)", func(t *testing.T) {
		in := baseInput()
		in.Cluster.Spec.DisableOperatorUser = true
		in.Cluster.Spec.Config = jsonExt(`{"credentials":{"users":{"replicator":{"roles":["replication"]}}}}`)

		if s := introspect(t, in); s.SuperUser != "" {
			t.Errorf("SuperUser = %q, want \"\"", s.SuperUser)
		}
	})

	t.Run("off mode: OffModeRW follows the instance-scope database.mode", func(t *testing.T) {
		// Default: the renderer makes ordinal 0 writable.
		in := baseInput()
		in.Cluster.Spec.Config = jsonExt(`{"replication":{"failover":"off"}}`)
		if s := introspect(t, in); s.OffModeRW != "storage-001-0" {
			t.Errorf("OffModeRW = %q, want storage-001-0 (renderer default)", s.OffModeRW)
		}

		// The user designates ordinal 1 instead; the operator must follow it
		// (status.leader and remediation protection key off this).
		in = baseInput()
		in.Cluster.Spec.Config = jsonExt(`{"replication":{"failover":"off"}}`)
		in.ReplicaSets[0].Spec.InstanceConfigs = map[string]apiextensionsv1.JSON{
			"1": {Raw: []byte(`{"database":{"mode":"rw"}}`)},
		}
		if s := introspect(t, in); s.OffModeRW != "storage-001-1" {
			t.Errorf("OffModeRW = %q, want storage-001-1 (user-designated rw)", s.OffModeRW)
		}

		// Election mode has no declared writable instance.
		if s := introspect(t, baseInput()); s.OffModeRW != "" {
			t.Errorf("OffModeRW = %q, want \"\" outside off mode", s.OffModeRW)
		}
	})

	t.Run("group-scope failover is honored (same precedence as the renderer)", func(t *testing.T) {
		in := baseInput()
		in.Cluster.Spec.Config = jsonExt(`{"replication":{"failover":"off"}}`)
		in.Cluster.Spec.GroupConfigs = map[string]apiextensionsv1.JSON{
			"storages": {Raw: []byte(`{"replication":{"failover":"election"}}`)},
		}

		if s := introspect(t, in); s.Failover != "election" {
			t.Errorf("Failover = %q, want election (group scope overrides global)", s.Failover)
		}
	})

	t.Run("defaults when keys are absent", func(t *testing.T) {
		d, err := clusterconfig.ParseDelivered([]byte("groups: {}\n"))
		if err != nil {
			t.Fatalf("ParseDelivered() error = %v", err)
		}
		s := d.Settings("storages", "storage-001")
		if s.Failover != "off" || s.Leader != "" || s.Port != 3301 {
			t.Errorf("defaults = %+v, want off/\"\"/3301", s)
		}
	})

	t.Run("garbage input errors", func(t *testing.T) {
		if _, err := clusterconfig.ParseDelivered([]byte("\tnot yaml")); err == nil {
			t.Error("expected a parse error")
		}
	})
}

// TestScopedRawConfigs: groupConfigs land at the group node and instanceConfigs
// at the matching instance node; operator-generated keys stay authoritative;
// unknown group names / out-of-range ordinals are ignored.
func TestScopedRawConfigs(t *testing.T) {
	in := baseInput() // storages/storage-001, 2 replicas
	in.Cluster.Spec.GroupConfigs = map[string]apiextensionsv1.JSON{
		"storages": {Raw: []byte(`{"memtx":{"memory":123},"replication":{"failover":"manual"}}`)},
		"missing":  {Raw: []byte(`{"log":{"level":7}}`)},
	}
	in.ReplicaSets[0].Spec.InstanceConfigs = map[string]apiextensionsv1.JSON{
		"1": {Raw: []byte(`{"memtx":{"memory":456},"iproto":{"advertise":{"peer":{"uri":"hacked"}}}}`)},
		"9": {Raw: []byte(`{"log":{"level":7}}`)},
	}

	tree := renderTree(t, in)
	groups := submap(t, tree, "groups")
	storages := submap(t, groups, "storages")

	// Group scope: raw keys present; Tarantool will apply precedence below it.
	if submap(t, storages, "memtx")["memory"] != float64(123) {
		t.Errorf("group-scope memtx.memory missing: %v", storages["memtx"])
	}
	if submap(t, storages, "replication")["failover"] != "manual" {
		t.Errorf("group-scope replication.failover missing")
	}
	if _, ok := groups["missing"]; ok {
		t.Errorf("groupConfigs for a group with no replica sets must be ignored")
	}

	insts := submap(t, submap(t, submap(t, storages, "replicasets"), "storage-001"), "instances")
	inst1 := submap(t, insts, "storage-001-1")
	if submap(t, inst1, "memtx")["memory"] != float64(456) {
		t.Errorf("instance-scope memtx.memory missing: %v", inst1["memtx"])
	}
	// Operator-generated advertise URI must win over the raw instance config.
	uri := submap(t, submap(t, submap(t, inst1, "iproto"), "advertise"), "peer")["uri"]
	if uri == "hacked" {
		t.Errorf("operator-generated advertise URI must not be overridable")
	}
	if _, ok := insts["storage-001-9"]; ok {
		t.Errorf("out-of-range instanceConfigs ordinal must be ignored")
	}
}

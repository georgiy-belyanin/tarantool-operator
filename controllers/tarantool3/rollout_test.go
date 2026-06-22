package tarantool3

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

func TestOperatorManagedRollout(t *testing.T) {
	rs := func(strategy appsv1.StatefulSetUpdateStrategyType) *v2alpha1.ReplicaSet {
		return &v2alpha1.ReplicaSet{Spec: v2alpha1.ReplicaSetSpec{
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: strategy},
		}}
	}
	cases := []struct {
		name     string
		rs       *v2alpha1.ReplicaSet
		settings clusterconfig.Settings
		want     bool
	}{
		{"election + super user", rs(""), clusterconfig.Settings{Failover: "election", SuperUser: "admin"}, true},
		{"supervised + super user", rs(""), clusterconfig.Settings{Failover: "supervised", SuperUser: "admin"}, true},
		{"manual: StatefulSet rolls", rs(""), clusterconfig.Settings{Failover: "manual", SuperUser: "admin"}, false},
		{"off: StatefulSet rolls", rs(""), clusterconfig.Settings{Failover: "off", SuperUser: "admin"}, false},
		{"no super user: leader unobservable", rs(""), clusterconfig.Settings{Failover: "election"}, false},
		{"explicit user strategy wins", rs(appsv1.RollingUpdateStatefulSetStrategyType), clusterconfig.Settings{Failover: "election", SuperUser: "admin"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := operatorManagedRollout(tc.rs, tc.settings); got != tc.want {
				t.Errorf("operatorManagedRollout = %v, want %v", got, tc.want)
			}
		})
	}
}

// rolloutFixture: a 3-instance set mid-rollout. Pods carry the
// controller-revision-hash label; "new" is the update revision.
type rolloutFixture struct {
	r   *ReplicaSetReconciler
	rs  *v2alpha1.ReplicaSet
	rec *record.FakeRecorder
}

func mkRolloutFixture(t *testing.T, readyReplicas int32, revisions map[int]string) rolloutFixture {
	t.Helper()
	replicas := int32(3)
	rs := &v2alpha1.ReplicaSet{Spec: v2alpha1.ReplicaSetSpec{Replicas: &replicas}}
	rs.Name = "roll-rs"
	rs.Namespace = "default"

	sts := &appsv1.StatefulSet{}
	sts.Name = "roll-rs"
	sts.Namespace = "default"
	sts.Spec.Replicas = &replicas
	sts.Status.UpdateRevision = "new"
	sts.Status.ReadyReplicas = readyReplicas

	objs := []client.Object{sts}
	for ordinal, rev := range revisions {
		pod := &corev1.Pod{}
		pod.Name = rs.InstanceName(int32(ordinal))
		pod.Namespace = "default"
		pod.Labels = map[string]string{
			ReplicaSetLabel:                       rs.Name,
			appsv1.ControllerRevisionHashLabelKey: rev,
		}
		objs = append(objs, pod)
	}

	rec := record.NewFakeRecorder(8)
	c := fake.NewClientBuilder().WithScheme(mappingScheme(t)).WithObjects(objs...).Build()
	return rolloutFixture{r: &ReplicaSetReconciler{Client: c, Recorder: rec}, rs: rs, rec: rec}
}

func (f rolloutFixture) podExists(t *testing.T, ordinal int) bool {
	t.Helper()
	err := f.r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: f.rs.InstanceName(int32(ordinal))}, &corev1.Pod{})
	return err == nil
}

func TestOrchestrateRollout(t *testing.T) {
	ctx := context.Background()

	t.Run("followers roll first, highest ordinal first", func(t *testing.T) {
		// All three stale; the leader is ordinal 1 — ordinal 2 must go first.
		f := mkRolloutFixture(t, 3, map[int]string{0: "old", 1: "old", 2: "old"})
		if !f.r.orchestrateRollout(ctx, f.rs, "roll-rs-1") {
			t.Fatalf("roll in progress must report true")
		}
		if f.podExists(t, 2) || !f.podExists(t, 1) || !f.podExists(t, 0) {
			t.Errorf("expected only ordinal 2 (highest stale follower) to be deleted")
		}
	})

	t.Run("leader is deferred while any follower is stale", func(t *testing.T) {
		// Leader at the HIGHEST ordinal: the default StatefulSet order would
		// restart it first; the orchestrator must pick the follower instead.
		f := mkRolloutFixture(t, 3, map[int]string{0: "old", 2: "old", 1: "new"})
		_ = f.r.orchestrateRollout(ctx, f.rs, "roll-rs-2")
		if f.podExists(t, 0) || !f.podExists(t, 2) {
			t.Errorf("expected follower 0 deleted and leader 2 deferred")
		}
	})

	t.Run("leader rolls when it is the only stale pod", func(t *testing.T) {
		f := mkRolloutFixture(t, 3, map[int]string{0: "new", 1: "old", 2: "new"})
		_ = f.r.orchestrateRollout(ctx, f.rs, "roll-rs-1")
		if f.podExists(t, 1) {
			t.Errorf("the leader must roll once it is the last stale pod")
		}
	})

	t.Run("no step unless every instance is ready", func(t *testing.T) {
		f := mkRolloutFixture(t, 2, map[int]string{0: "old", 1: "old", 2: "old"})
		if !f.r.orchestrateRollout(ctx, f.rs, "roll-rs-1") {
			t.Fatalf("unfinished roll must report in progress")
		}
		if !f.podExists(t, 0) || !f.podExists(t, 1) || !f.podExists(t, 2) {
			t.Errorf("no pod may be deleted while the set is not fully ready")
		}
	})

	t.Run("converged set is a no-op", func(t *testing.T) {
		f := mkRolloutFixture(t, 3, map[int]string{0: "new", 1: "new", 2: "new"})
		if f.r.orchestrateRollout(ctx, f.rs, "roll-rs-1") {
			t.Errorf("converged roll must report false (no requeue)")
		}
		if !f.podExists(t, 0) || !f.podExists(t, 1) || !f.podExists(t, 2) {
			t.Errorf("converged roll must not delete anything")
		}
	})

	t.Run("unknown leader degrades to ordinal order", func(t *testing.T) {
		f := mkRolloutFixture(t, 3, map[int]string{1: "old", 2: "old", 0: "new"})
		_ = f.r.orchestrateRollout(ctx, f.rs, "")
		if f.podExists(t, 2) || !f.podExists(t, 1) {
			t.Errorf("with no observed leader, the highest stale ordinal rolls first")
		}
	})

	t.Run("emits a RollingOut event naming the role", func(t *testing.T) {
		f := mkRolloutFixture(t, 3, map[int]string{0: "new", 1: "old", 2: "new"})
		_ = f.r.orchestrateRollout(ctx, f.rs, "roll-rs-1")
		select {
		case ev := <-f.rec.Events:
			if !strings.Contains(ev, "RollingOut") || !strings.Contains(ev, "leader") {
				t.Errorf("event %q should name the rollout and the leader role", ev)
			}
		default:
			t.Errorf("expected a RollingOut event")
		}
	})
}

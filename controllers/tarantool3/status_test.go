package tarantool3

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// TestMembersAvailableCondition: the Cluster-level data-plane aggregate is True
// only when every child ReplicaSet reports Available, and the message carries
// the counts.
func TestMembersAvailableCondition(t *testing.T) {
	rsWithAvailable := func(name string, available bool) v2alpha1.ReplicaSet {
		status := metav1.ConditionFalse
		if available {
			status = metav1.ConditionTrue
		}
		rs := v2alpha1.ReplicaSet{}
		rs.Name = name
		rs.Status.Conditions = []metav1.Condition{{Type: ConditionAvailable, Status: status, Reason: "x"}}
		return rs
	}

	t.Run("all available", func(t *testing.T) {
		c := membersAvailableCondition([]v2alpha1.ReplicaSet{rsWithAvailable("a", true), rsWithAvailable("b", true)}, 1)
		if c.Status != metav1.ConditionTrue || c.Reason != "AllReplicaSetsAvailable" {
			t.Errorf("got %+v, want True/AllReplicaSetsAvailable", c)
		}
	})
	t.Run("one unavailable", func(t *testing.T) {
		c := membersAvailableCondition([]v2alpha1.ReplicaSet{rsWithAvailable("a", true), rsWithAvailable("b", false)}, 1)
		if c.Status != metav1.ConditionFalse || c.Reason != "ReplicaSetUnavailable" {
			t.Errorf("got %+v, want False/ReplicaSetUnavailable", c)
		}
		if want := "1/2 replica sets have a quorum of ready instances"; c.Message != want {
			t.Errorf("message %q, want %q", c.Message, want)
		}
	})
	t.Run("no replica sets", func(t *testing.T) {
		if c := membersAvailableCondition(nil, 1); c.Status != metav1.ConditionFalse || c.Reason != "NoReplicaSets" {
			t.Errorf("got %+v, want False/NoReplicaSets", c)
		}
	})
}

// TestObserveLeaderManual covers the no-network path: in manual failover the
// leader comes straight from the spec (the client is never used).
func TestObserveLeaderManual(t *testing.T) {
	r := &ReplicaSetReconciler{} // nil Client is fine: manual path makes no calls
	cluster := &v2alpha1.Cluster{}
	rs := &v2alpha1.ReplicaSet{}

	if got := r.observeLeader(context.Background(), cluster, rs, clusterconfig.Settings{Failover: "manual", Leader: "storage-001-1", Port: 3301}); got != "storage-001-1" {
		t.Errorf("observeLeader(manual) = %q, want storage-001-1", got)
	}
}

// TestObserveLeaderNoSuperUser: election mode with no super user can't read
// box.info, so it returns "" without touching the (nil) client.
func TestObserveLeaderNoSuperUser(t *testing.T) {
	r := &ReplicaSetReconciler{}
	cluster := &v2alpha1.Cluster{}
	rs := &v2alpha1.ReplicaSet{Spec: v2alpha1.ReplicaSetSpec{Replicas: int32p(2)}}

	if got := r.observeLeader(context.Background(), cluster, rs, clusterconfig.Settings{Failover: "election", Port: 3301}); got != "" {
		t.Errorf("observeLeader(no super user) = %q, want \"\"", got)
	}
}

// TestObserveLeaderPasswordResolveFails: the config declares a super user but
// the cluster names no credentialsSecret, so the password can't be resolved and
// the leader read is skipped (returns "" without any network call — nil client
// is safe).
func TestObserveLeaderPasswordResolveFails(t *testing.T) {
	r := &ReplicaSetReconciler{}
	cluster := &v2alpha1.Cluster{} // no credentialsSecret
	rs := &v2alpha1.ReplicaSet{Spec: v2alpha1.ReplicaSetSpec{Replicas: int32p(1)}}

	if got := r.observeLeader(context.Background(), cluster, rs, clusterconfig.Settings{Failover: "election", Port: 3301, SuperUser: "admin"}); got != "" {
		t.Errorf("observeLeader = %q, want \"\" when the super-user password cannot be resolved", got)
	}
}

func TestResolveUserPassword(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "tarantool"},
		Data:       map[string][]byte{"admin": []byte("s3cr3t")},
	}
	cluster := func(secretName string) *v2alpha1.Cluster {
		return &v2alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "tarantool"},
			Spec:       v2alpha1.ClusterSpec{CredentialsSecret: secretName},
		}
	}

	c := fake.NewClientBuilder().WithObjects(secret).Build()
	ctx := context.Background()

	t.Run("success", func(t *testing.T) {
		got, err := resolveUserPassword(ctx, c, cluster("creds"), "admin")
		if err != nil || got != "s3cr3t" {
			t.Errorf("resolveUserPassword = %q, %v; want s3cr3t, nil", got, err)
		}
	})
	t.Run("user missing from secret", func(t *testing.T) {
		if _, err := resolveUserPassword(ctx, c, cluster("creds"), "ghost"); err == nil {
			t.Errorf("want error for a user with no key in the secret")
		}
	})
	t.Run("no credentialsSecret", func(t *testing.T) {
		if _, err := resolveUserPassword(ctx, c, cluster(""), "admin"); err == nil {
			t.Errorf("want error when the cluster names no credentialsSecret")
		}
	})
	t.Run("missing secret", func(t *testing.T) {
		if _, err := resolveUserPassword(ctx, c, cluster("absent"), "admin"); err == nil {
			t.Errorf("want error when the secret does not exist")
		}
	})
}

// TestReplicaSetConditions covers the Available/Progressing/Degraded triad (F20).
func TestReplicaSetConditions(t *testing.T) {
	mk := func(replicas int32, bootstrapped bool) *v2alpha1.ReplicaSet {
		rs := &v2alpha1.ReplicaSet{Spec: v2alpha1.ReplicaSetSpec{Replicas: &replicas}}
		rs.Status.Bootstrapped = bootstrapped
		return rs
	}
	get := func(conds []metav1.Condition, typ string) metav1.ConditionStatus {
		for _, c := range conds {
			if c.Type == typ {
				return c.Status
			}
		}
		return "missing"
	}
	cases := []struct {
		name                 string
		rs                   *v2alpha1.ReplicaSet
		ready                int32
		avail, progr, degrad metav1.ConditionStatus
	}{
		{"fresh 3, none ready", mk(3, false), 0, metav1.ConditionFalse, metav1.ConditionTrue, metav1.ConditionFalse},
		{"3 with quorum, converging", mk(3, false), 2, metav1.ConditionTrue, metav1.ConditionTrue, metav1.ConditionFalse},
		{"3 fully ready", mk(3, true), 3, metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionFalse},
		{"bootstrapped 3, quorum lost", mk(3, true), 1, metav1.ConditionFalse, metav1.ConditionTrue, metav1.ConditionTrue},
		{"single ready", mk(1, true), 1, metav1.ConditionTrue, metav1.ConditionFalse, metav1.ConditionFalse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conds := replicaSetConditions(tc.rs, tc.ready)
			if got := get(conds, ConditionAvailable); got != tc.avail {
				t.Errorf("Available = %s, want %s", got, tc.avail)
			}
			if got := get(conds, ConditionProgressing); got != tc.progr {
				t.Errorf("Progressing = %s, want %s", got, tc.progr)
			}
			if got := get(conds, ConditionDegraded); got != tc.degrad {
				t.Errorf("Degraded = %s, want %s", got, tc.degrad)
			}
		})
	}
}

// TestMetricsRegistered: the operator metrics are registered and incrementable.
func TestMetricsRegistered(t *testing.T) {
	before := testutil.ToFloat64(renderTotal.WithLabelValues("ok"))
	renderTotal.WithLabelValues("ok").Inc()
	if got := testutil.ToFloat64(renderTotal.WithLabelValues("ok")); got != before+1 {
		t.Errorf("renderTotal ok = %v, want %v", got, before+1)
	}
	reloadRetryTotal.Inc()
	leaderObservationFailuresTotal.Inc()
	if testutil.ToFloat64(reloadRetryTotal) < 1 || testutil.ToFloat64(leaderObservationFailuresTotal) < 1 {
		t.Errorf("reload/leader metrics did not increment")
	}
}

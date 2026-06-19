package tarantool3

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// electionClusterWithSuper builds a cluster whose credentialsSecret holds the
// super user's password, so observeLeader proceeds to the network path (the
// super user itself comes from the introspected Settings).
func electionClusterWithSuper(t *testing.T) (*v2alpha1.Cluster, *ReplicaSetReconciler) {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "ns1"},
		Data:       map[string][]byte{"admin": []byte("p")},
	}
	cluster := &v2alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "ns1"},
		Spec:       v2alpha1.ClusterSpec{CredentialsSecret: "creds"},
	}
	c := fake.NewClientBuilder().WithScheme(mappingScheme(t)).WithObjects(secret).Build()
	return cluster, &ReplicaSetReconciler{Client: c}
}

// TestObserveLeaderBudgetBounded is the regression test for RP5 (REVIEW.md):
// observeLeader must not block the reconcile for replicas × connectTimeout. With
// a reader that hangs until its context is cancelled, observeLeader over many
// replicas must still return within roughly observeBudget — not 50 × the dial
// timeout — and it must not call the reader once per replica.
func TestObserveLeaderBudgetBounded(t *testing.T) {
	orig := evalInstanceString
	t.Cleanup(func() { evalInstanceString = orig })

	var calls int
	evalInstanceString = func(ctx context.Context, _, _, _, _ string, _ time.Duration) (string, error) {
		calls++
		<-ctx.Done() // simulate an unreachable instance that hangs until cut off
		return "", ctx.Err()
	}

	cluster, r := electionClusterWithSuper(t)
	rs := &v2alpha1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-001", Namespace: "ns1"},
		Spec:       v2alpha1.ReplicaSetSpec{ClusterName: "example", Replicas: int32p(50)},
	}

	start := time.Now()
	got := r.observeLeader(context.Background(), cluster, rs, clusterconfig.Settings{Failover: "election", Port: 3301, SuperUser: "admin"})
	elapsed := time.Since(start)

	if got != "" {
		t.Errorf("observeLeader = %q, want \"\" when no instance answers", got)
	}
	// Must be bounded by ~observeBudget, nowhere near 50 × connectTimeout.
	if elapsed > observeBudget+2*time.Second {
		t.Errorf("observeLeader took %v, want ≲ observeBudget (%v) — not replicas × timeout", elapsed, observeBudget)
	}
	// The budget guard must stop the loop well before 50 attempts.
	if calls >= 50 {
		t.Errorf("reader called %d times; the budget should cap attempts well below replicas", calls)
	}
}

// TestObserveLeaderAlreadyCancelled: a cancelled parent context short-circuits
// the loop without any network attempt.
func TestObserveLeaderAlreadyCancelled(t *testing.T) {
	orig := evalInstanceString
	t.Cleanup(func() { evalInstanceString = orig })
	var calls int
	evalInstanceString = func(context.Context, string, string, string, string, time.Duration) (string, error) {
		calls++
		return "", nil
	}

	cluster, r := electionClusterWithSuper(t)
	rs := &v2alpha1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-001", Namespace: "ns1"},
		Spec:       v2alpha1.ReplicaSetSpec{ClusterName: "example", Replicas: int32p(3)},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := r.observeLeader(ctx, cluster, rs, clusterconfig.Settings{Failover: "election", Port: 3301, SuperUser: "admin"}); got != "" {
		t.Errorf("observeLeader(cancelled) = %q, want \"\"", got)
	}
	if calls != 0 {
		t.Errorf("reader called %d times on a cancelled context, want 0", calls)
	}
}

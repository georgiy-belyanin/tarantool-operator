package tarantool3

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// shardDeleteFixture builds a deleting storage replica set (drain finalizer held)
// plus its Cluster (declaring an admin super user, so settings.SuperUser != "")
// and a rendered config Secret, on a status-enabled fake client.
func shardDeleteFixture(t *testing.T) (*ReplicaSetReconciler, *v2alpha1.ReplicaSet) {
	t.Helper()
	cluster := &v2alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "shard", Namespace: "default"},
		Spec: v2alpha1.ClusterSpec{
			CredentialsSecret: "creds",
			Config:            rawJSON(`{"credentials":{"users":{"admin":{"roles":["super"]}}},"sharding":{"bucket_count":100}}`),
			// This fixture exercises the user-managed super-user flow; the
			// built-in operator user would need its generated Secret instead.
			DisableOperatorUser: true,
		},
	}
	replicas := int32(1)
	now := metav1.Now()
	rs := &v2alpha1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "storage-c", Namespace: "default",
			Finalizers:        []string{ShardDrainFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: v2alpha1.ReplicaSetSpec{
			ClusterName: "shard", Group: "storages", Replicas: &replicas,
			Config: rawJSON(`{"sharding":{"roles":["storage"],"weight":1}}`),
		},
	}
	rendered, hash, err := clusterconfig.RenderAndHash(clusterconfig.Input{
		Cluster: cluster, ReplicaSets: []v2alpha1.ReplicaSet{*rs},
		Passwords: map[string]string{"admin": "p"},
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "shard-config", Namespace: "default", Annotations: map[string]string{ConfigHashAnnotation: hash}},
		Data:       map[string][]byte{ConfigFileName: rendered},
	}
	credSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
		Data:       map[string][]byte{"admin": []byte("p")},
	}
	c := fake.NewClientBuilder().
		WithScheme(mappingScheme(t)).
		WithStatusSubresource(&v2alpha1.ReplicaSet{}).
		WithObjects(cluster, rs, secret, credSecret).
		Build()
	return &ReplicaSetReconciler{Client: c, Recorder: record.NewFakeRecorder(8)}, rs
}

func shardFinalizerHeld(t *testing.T, r *ReplicaSetReconciler) bool {
	t.Helper()
	got := &v2alpha1.ReplicaSet{}
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "storage-c"}, got); err != nil {
		return false // gone => released and deleted
	}
	return controllerutil.ContainsFinalizer(got, ShardDrainFinalizer)
}

func TestReconcileDeleteDrainGate(t *testing.T) {
	ctx := context.Background()

	t.Run("buckets remain: finalizer held, requeue", func(t *testing.T) {
		orig := evalInstanceString
		t.Cleanup(func() { evalInstanceString = orig })
		evalInstanceString = func(context.Context, string, string, string, string, time.Duration) (string, error) {
			return "42", nil // still owns buckets
		}
		r, rs := shardDeleteFixture(t)
		res, err := r.reconcileDelete(ctx, rs)
		if err != nil {
			t.Fatalf("reconcileDelete: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Errorf("want a requeue while buckets remain")
		}
		if !shardFinalizerHeld(t, r) {
			t.Errorf("finalizer must be held while buckets remain")
		}
	})

	t.Run("drained: finalizer released", func(t *testing.T) {
		orig := evalInstanceString
		t.Cleanup(func() { evalInstanceString = orig })
		evalInstanceString = func(context.Context, string, string, string, string, time.Duration) (string, error) {
			return "0", nil // fully drained
		}
		r, rs := shardDeleteFixture(t)
		if _, err := r.reconcileDelete(ctx, rs); err != nil {
			t.Fatalf("reconcileDelete: %v", err)
		}
		if shardFinalizerHeld(t, r) {
			t.Errorf("finalizer must be released once drained")
		}
	})

	t.Run("unreachable instance: finalizer held (cannot confirm drain)", func(t *testing.T) {
		orig := evalInstanceString
		t.Cleanup(func() { evalInstanceString = orig })
		evalInstanceString = func(context.Context, string, string, string, string, time.Duration) (string, error) {
			return "", context.DeadlineExceeded
		}
		r, rs := shardDeleteFixture(t)
		if _, err := r.reconcileDelete(ctx, rs); err != nil {
			t.Fatalf("reconcileDelete: %v", err)
		}
		if !shardFinalizerHeld(t, r) {
			t.Errorf("finalizer must be held when the drain cannot be confirmed")
		}
	})

	t.Run("cluster gone: finalizer released (teardown)", func(t *testing.T) {
		r, rs := shardDeleteFixture(t)
		cl := &v2alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "shard", Namespace: "default"}}
		_ = r.Delete(ctx, cl)
		if _, err := r.reconcileDelete(ctx, rs); err != nil {
			t.Fatalf("reconcileDelete: %v", err)
		}
		if shardFinalizerHeld(t, r) {
			t.Errorf("finalizer must be released when the cluster is gone")
		}
	})

	t.Run("no finalizer: plain GC, no-op", func(t *testing.T) {
		r, rs := shardDeleteFixture(t)
		rs.Finalizers = nil
		res, err := r.reconcileDelete(ctx, rs)
		if err != nil || res.RequeueAfter != 0 {
			t.Errorf("reconcileDelete without the finalizer should be a no-op, got %v %v", res, err)
		}
	})
}

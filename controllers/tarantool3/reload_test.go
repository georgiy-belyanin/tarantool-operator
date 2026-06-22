package tarantool3

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// superSettings mimics Delivered.Settings on a config declaring an "admin"
// user with the super role.
func superSettings() clusterconfig.Settings {
	return clusterconfig.Settings{Failover: "election", Port: 3301, SuperUser: "admin"}
}

func noSuperSettings() clusterconfig.Settings {
	return clusterconfig.Settings{Failover: "election", Port: 3301}
}

func superCluster() *v2alpha1.Cluster {
	return &v2alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"},
		Spec:       v2alpha1.ClusterSpec{CredentialsSecret: "creds"},
	}
}

func TestReloadConfig(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
		Data:       map[string][]byte{"admin": []byte("pw")},
	}
	cl := superCluster()
	rs := &v2alpha1.ReplicaSet{Spec: v2alpha1.ReplicaSetSpec{Replicas: int32p(3)}}
	rs.Name = "storage-001"
	rs.Namespace = "default"
	r := &ReplicaSetReconciler{Client: fake.NewClientBuilder().WithObjects(secret).Build()}

	orig := evalInstanceString
	defer func() { evalInstanceString = orig }()

	t.Run("all converged", func(t *testing.T) {
		evalInstanceString = func(_ context.Context, _, _, _, _ string, _ time.Duration) (string, error) { return "H", nil }
		if _, retry, _ := r.reloadConfig(context.Background(), cl, rs, superSettings(), "H", rs.GetReplicas()); retry {
			t.Errorf("got retry=%v, want false (all instances already converged)", retry)
		}
	})
	t.Run("reachable but stale -> retry", func(t *testing.T) {
		evalInstanceString = func(_ context.Context, _, _, _, _ string, _ time.Duration) (string, error) { return "OLD", nil }
		if _, retry, _ := r.reloadConfig(context.Background(), cl, rs, superSettings(), "H", rs.GetReplicas()); !retry {
			t.Errorf("got retry=%v, want true (reachable but stale)", retry)
		}
	})
	t.Run("unreachable -> no retry", func(t *testing.T) {
		evalInstanceString = func(_ context.Context, _, _, _, _ string, _ time.Duration) (string, error) {
			return "", errors.New("dial")
		}
		if _, retry, _ := r.reloadConfig(context.Background(), cl, rs, superSettings(), "H", rs.GetReplicas()); retry {
			t.Errorf("got retry=%v, want false (unreachable is not worth a hot requeue)", retry)
		}
	})
	t.Run("apply failure -> reason returned for the caller to surface", func(t *testing.T) {
		// The eval script reports "<loaded>\t<reason>" when config:reload() failed
		// or left alerts; reloadConfig returns that reason (the caller surfaces it
		// as the Degraded condition + a ConfigRejected event).
		evalInstanceString = func(_ context.Context, _, _, _, _ string, _ time.Duration) (string, error) {
			return "OLD\tIncorrect value of option 'memtx_memory'", nil
		}
		_, retry, applyErr := r.reloadConfig(context.Background(), cl, rs, superSettings(), "H", rs.GetReplicas())
		if !retry {
			t.Errorf("got retry=%v, want true (apply failure leaves the instance stale)", retry)
		}
		if !strings.Contains(applyErr, "memtx_memory") {
			t.Errorf("applyErr %q should carry Tarantool's reason", applyErr)
		}
	})
	t.Run("no super user -> noop", func(t *testing.T) {
		called := false
		evalInstanceString = func(_ context.Context, _, _, _, _ string, _ time.Duration) (string, error) {
			called = true
			return "H", nil
		}
		nosuper := &v2alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
		_, retry, _ := r.reloadConfig(context.Background(), nosuper, rs, noSuperSettings(), "H", rs.GetReplicas())
		if retry || called {
			t.Errorf("expected no-op without a super user (retry=%v called=%v)", retry, called)
		}
	})
}

// TestScaleUpReplicas: scaling UP is held at the StatefulSet's current size
// until the delivered config lists every desired instance AND the existing
// instances confirm they loaded it — otherwise a new pod joins a master whose
// loaded config does not know it, and the master's autoexpel expels the
// newcomer on the spot (verified live on kind). Scale-down, initial creation
// and the no-super-user path are never held.
func TestScaleUpReplicas(t *testing.T) {
	ctx := context.Background()
	mkRS := func(replicas int32) *v2alpha1.ReplicaSet {
		rs := &v2alpha1.ReplicaSet{Spec: v2alpha1.ReplicaSetSpec{
			ClusterName: "c", Group: "storages", Replicas: &replicas,
		}}
		rs.Name = "storage-001"
		rs.Namespace = "default"
		return rs
	}
	mkSTS := func(replicas int32) *appsv1.StatefulSet {
		sts := &appsv1.StatefulSet{}
		sts.Name = "storage-001"
		sts.Namespace = "default"
		sts.Spec.Replicas = &replicas
		return sts
	}
	// deliveredWith renders a config whose topology lists n instances.
	deliveredWith := func(t *testing.T, n int32) *clusterconfig.Delivered {
		t.Helper()
		rendered, err := clusterconfig.Render(clusterconfig.Input{
			Cluster:     superCluster(),
			ReplicaSets: []v2alpha1.ReplicaSet{*mkRS(n)},
		})
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		d, err := clusterconfig.ParseDelivered(rendered)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return d
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
		Data:       map[string][]byte{"admin": []byte("pw")},
	}
	mkReconciler := func(objs ...client.Object) *ReplicaSetReconciler {
		return &ReplicaSetReconciler{Client: fake.NewClientBuilder().WithObjects(objs...).Build()}
	}
	orig := evalInstanceString
	defer func() { evalInstanceString = orig }()

	t.Run("held while the delivered topology lags", func(t *testing.T) {
		r := mkReconciler(secret, mkSTS(1))
		got, held, err := r.scaleUpReplicas(ctx, superCluster(), mkRS(3), superSettings(), deliveredWith(t, 1), "H")
		if err != nil || got != 1 || !held {
			t.Errorf("got (%d,%v,%v), want (1,true,nil): delivered config lists only 1 instance", got, held, err)
		}
	})

	t.Run("held until existing instances load the new config", func(t *testing.T) {
		evalInstanceString = func(_ context.Context, _, _, _, _ string, _ time.Duration) (string, error) {
			return "OLD", nil // existing instance still runs the previous version
		}
		r := mkReconciler(secret, mkSTS(1))
		got, held, err := r.scaleUpReplicas(ctx, superCluster(), mkRS(3), superSettings(), deliveredWith(t, 3), "H")
		if err != nil || got != 1 || !held {
			t.Errorf("got (%d,%v,%v), want (1,true,nil): existing instance not converged", got, held, err)
		}
	})

	t.Run("proceeds once delivered and loaded", func(t *testing.T) {
		evalInstanceString = func(_ context.Context, _, _, _, _ string, _ time.Duration) (string, error) {
			return "H", nil
		}
		r := mkReconciler(secret, mkSTS(1))
		got, held, err := r.scaleUpReplicas(ctx, superCluster(), mkRS(3), superSettings(), deliveredWith(t, 3), "H")
		if err != nil || got != 3 || held {
			t.Errorf("got (%d,%v,%v), want (3,false,nil)", got, held, err)
		}
	})

	t.Run("scale-down is never held", func(t *testing.T) {
		r := mkReconciler(secret, mkSTS(3))
		got, held, err := r.scaleUpReplicas(ctx, superCluster(), mkRS(1), superSettings(), deliveredWith(t, 1), "H")
		if err != nil || got != 1 || held {
			t.Errorf("scale-down: got (%d,%v,%v), want (1,false,nil)", got, held, err)
		}
	})

	t.Run("initial creation is not held", func(t *testing.T) {
		r := mkReconciler(secret) // no StatefulSet yet: joint bootstrap
		got, held, err := r.scaleUpReplicas(ctx, superCluster(), mkRS(3), superSettings(), deliveredWith(t, 3), "H")
		if err != nil || got != 3 || held {
			t.Errorf("initial: got (%d,%v,%v), want (3,false,nil)", got, held, err)
		}
	})

	t.Run("no super user: scaling rolls, never held", func(t *testing.T) {
		r := mkReconciler(secret, mkSTS(1))
		got, held, err := r.scaleUpReplicas(ctx, superCluster(), mkRS(3), noSuperSettings(), deliveredWith(t, 1), "H")
		if err != nil || got != 3 || held {
			t.Errorf("no-super: got (%d,%v,%v), want (3,false,nil)", got, held, err)
		}
	})
}

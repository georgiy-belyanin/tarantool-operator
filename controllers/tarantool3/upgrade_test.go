package tarantool3

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
)

// fakeEval substitutes evalInstanceString: versions maps ordinal-suffix (e.g.
// "-0") to box.info.version; leaderOrdinal is the only instance whose upgrade
// script returns its version (others are read-only -> "").
func fakeEval(versions map[string]string, leaderSuffix string, upgraded *[]string) func(context.Context, string, string, string, string, time.Duration) (string, error) {
	return func(_ context.Context, addr, _, _, script string, _ time.Duration) (string, error) {
		var suffix string
		for s := range versions {
			if strings.Contains(addr, "up-rs"+s+".") {
				suffix = s
				break
			}
		}
		v, ok := versions[suffix]
		if !ok {
			return "", errors.New("unreachable")
		}
		if strings.Contains(script, "box.schema.upgrade") {
			if suffix == leaderSuffix {
				*upgraded = append(*upgraded, suffix)
				return v, nil
			}
			return "", nil // read-only follower: no-op
		}
		return v, nil // version read
	}
}

func upgradeFixture(t *testing.T, image string) (*ReplicaSetReconciler, *v2alpha1.Cluster, *v2alpha1.ReplicaSet) {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
		Data:       map[string][]byte{"admin": []byte("pw")},
	}
	cl := &v2alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"},
		Spec:       v2alpha1.ClusterSpec{CredentialsSecret: "creds"},
	}
	replicas := int32(3)
	rs := &v2alpha1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "up-rs", Namespace: "default"},
		Spec: v2alpha1.ReplicaSetSpec{
			ClusterName: "c", Replicas: &replicas,
			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "tarantool", Image: image}},
			}},
		},
	}
	return &ReplicaSetReconciler{Client: fake.NewClientBuilder().WithObjects(secret).Build(), Recorder: record.NewFakeRecorder(8)}, cl, rs
}

func TestMaybeUpgradeSchema(t *testing.T) {
	ctx := context.Background()
	orig := evalInstanceString
	defer func() { evalInstanceString = orig }()

	t.Run("homogeneous versions: upgrade runs once on the leader, status set", func(t *testing.T) {
		var upgraded []string
		evalInstanceString = fakeEval(map[string]string{"-0": "3.7.0", "-1": "3.7.0", "-2": "3.7.0"}, "-1", &upgraded)
		r, cl, rs := upgradeFixture(t, "tarantool/tarantool:3.7")
		if changed := r.maybeUpgradeSchema(ctx, cl, rs, superSettings(), 3); !changed {
			t.Fatalf("expected schema upgrade to run")
		}
		if len(upgraded) != 1 || upgraded[0] != "-1" {
			t.Errorf("upgrade should run exactly once, on the leader: %v", upgraded)
		}
		if rs.Status.SchemaUpgradedForImage != "tarantool/tarantool:3.7" {
			t.Errorf("SchemaUpgradedForImage = %q", rs.Status.SchemaUpgradedForImage)
		}
		// Second call with the same image: no-op.
		if r.maybeUpgradeSchema(ctx, cl, rs, superSettings(), 3) {
			t.Errorf("must not re-run for an already-upgraded image")
		}
	})

	t.Run("mixed versions: deferred", func(t *testing.T) {
		var upgraded []string
		evalInstanceString = fakeEval(map[string]string{"-0": "3.6.0", "-1": "3.7.0", "-2": "3.7.0"}, "-1", &upgraded)
		r, cl, rs := upgradeFixture(t, "tarantool/tarantool:3.7")
		if r.maybeUpgradeSchema(ctx, cl, rs, superSettings(), 3) || len(upgraded) != 0 {
			t.Errorf("schema must not move while binary versions are mixed: %v", upgraded)
		}
	})

	t.Run("rollout incomplete: deferred", func(t *testing.T) {
		var upgraded []string
		evalInstanceString = fakeEval(map[string]string{"-0": "3.7.0", "-1": "3.7.0", "-2": "3.7.0"}, "-0", &upgraded)
		r, cl, rs := upgradeFixture(t, "tarantool/tarantool:3.7")
		if r.maybeUpgradeSchema(ctx, cl, rs, superSettings(), 2) || len(upgraded) != 0 {
			t.Errorf("must wait for all instances to be ready")
		}
	})

	t.Run("instance unreachable: deferred", func(t *testing.T) {
		var upgraded []string
		evalInstanceString = fakeEval(map[string]string{"-0": "3.7.0", "-1": "3.7.0"}, "-0", &upgraded) // -2 missing
		r, cl, rs := upgradeFixture(t, "tarantool/tarantool:3.7")
		if r.maybeUpgradeSchema(ctx, cl, rs, superSettings(), 3) || len(upgraded) != 0 {
			t.Errorf("must defer when an instance version cannot be read")
		}
	})

	t.Run("no super user: no-op", func(t *testing.T) {
		called := false
		evalInstanceString = func(context.Context, string, string, string, string, time.Duration) (string, error) {
			called = true
			return "3.7.0", nil
		}
		r, _, rs := upgradeFixture(t, "tarantool/tarantool:3.7")
		nosuper := &v2alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}
		if r.maybeUpgradeSchema(ctx, nosuper, rs, noSuperSettings(), 3) || called {
			t.Errorf("must be a no-op without a super user")
		}
	})
}

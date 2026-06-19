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

func TestMaybeBootstrapVshard(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
		Data:       map[string][]byte{"admin": []byte("pw")},
	}
	newRS := func() *v2alpha1.ReplicaSet {
		rs := &v2alpha1.ReplicaSet{Spec: v2alpha1.ReplicaSetSpec{Replicas: int32p(2)}}
		rs.Name = "router-a"
		rs.Namespace = "default"
		return rs
	}
	mkR := func(rec record.EventRecorder) *ReplicaSetReconciler {
		return &ReplicaSetReconciler{Client: fake.NewClientBuilder().WithObjects(secret).Build(), Recorder: rec}
	}

	orig := evalInstanceString
	defer func() { evalInstanceString = orig }()

	t.Run("ok -> done, sets sticky flag, emits event", func(t *testing.T) {
		evalInstanceString = func(_ context.Context, _, _, _, _ string, _ time.Duration) (string, error) { return "ok", nil }
		rec := record.NewFakeRecorder(4)
		r, rs := mkR(rec), newRS()
		if retry := r.maybeBootstrapVshard(context.Background(), superCluster(), rs, superSettings(), 2); retry {
			t.Errorf("retry = true, want false (bootstrapped this pass)")
		}
		if !rs.Status.ShardingBootstrapped {
			t.Errorf("ShardingBootstrapped should be set after a successful bootstrap")
		}
		select {
		case ev := <-rec.Events:
			if !strings.Contains(ev, "VshardBootstrapped") {
				t.Errorf("event %q lacks VshardBootstrapped", ev)
			}
		default:
			t.Errorf("expected a VshardBootstrapped event")
		}
	})

	t.Run("retry while storages unreachable -> retry, no flag", func(t *testing.T) {
		evalInstanceString = func(_ context.Context, _, _, _, _ string, _ time.Duration) (string, error) {
			return "retry: replicaset is unreachable", nil
		}
		r, rs := mkR(record.NewFakeRecorder(4)), newRS()
		if retry := r.maybeBootstrapVshard(context.Background(), superCluster(), rs, superSettings(), 2); !retry {
			t.Errorf("retry = false, want true while storages unreachable")
		}
		if rs.Status.ShardingBootstrapped {
			t.Errorf("ShardingBootstrapped must NOT be set when bootstrap could not complete")
		}
	})

	t.Run("already bootstrapped -> noop, no iproto call", func(t *testing.T) {
		called := false
		evalInstanceString = func(_ context.Context, _, _, _, _ string, _ time.Duration) (string, error) {
			called = true
			return "ok", nil
		}
		r, rs := mkR(record.NewFakeRecorder(4)), newRS()
		rs.Status.ShardingBootstrapped = true
		if retry := r.maybeBootstrapVshard(context.Background(), superCluster(), rs, superSettings(), 2); retry {
			t.Errorf("retry = true, want false when already bootstrapped")
		}
		if called {
			t.Errorf("must not touch iproto when already bootstrapped")
		}
	})

	t.Run("no super user -> noop", func(t *testing.T) {
		called := false
		evalInstanceString = func(_ context.Context, _, _, _, _ string, _ time.Duration) (string, error) {
			called = true
			return "ok", nil
		}
		r, rs := mkR(record.NewFakeRecorder(4)), newRS()
		if retry := r.maybeBootstrapVshard(context.Background(), superCluster(), rs, noSuperSettings(), 2); retry || called {
			t.Errorf("want no retry and no iproto call without a super user (retry=%v, called=%v)", retry, called)
		}
	})

	t.Run("nothing ready -> noop", func(t *testing.T) {
		evalInstanceString = func(_ context.Context, _, _, _, _ string, _ time.Duration) (string, error) { return "ok", nil }
		r, rs := mkR(record.NewFakeRecorder(4)), newRS()
		if retry := r.maybeBootstrapVshard(context.Background(), superCluster(), rs, superSettings(), 0); retry {
			t.Errorf("retry = true, want false when no instance is ready")
		}
	})

	t.Run("unreachable router -> noop (retry next reconcile)", func(t *testing.T) {
		evalInstanceString = func(_ context.Context, _, _, _, _ string, _ time.Duration) (string, error) {
			return "", errors.New("dial")
		}
		r, rs := mkR(record.NewFakeRecorder(4)), newRS()
		if retry := r.maybeBootstrapVshard(context.Background(), superCluster(), rs, superSettings(), 2); retry {
			t.Errorf("retry = true, want false when the router is unreachable")
		}
	})
}

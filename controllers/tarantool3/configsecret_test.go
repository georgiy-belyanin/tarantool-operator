package tarantool3

import (
	"context"
	"strings"
	"testing"

	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
)

// recordedReason reports whether the fake recorder emitted an event whose reason
// substring is present (drains the channel non-blocking).
func recordedReason(rec *record.FakeRecorder, reason string) bool {
	for {
		select {
		case ev := <-rec.Events:
			if strings.Contains(ev, reason) {
				return true
			}
		default:
			return false
		}
	}
}

func memtxReconciler(t *testing.T) (*ClusterReconciler, *record.FakeRecorder) {
	t.Helper()
	sch := mappingScheme(t)
	rec := record.NewFakeRecorder(8)
	return &ClusterReconciler{
		Client:   fake.NewClientBuilder().WithScheme(sch).Build(),
		Scheme:   sch,
		Recorder: rec,
	}, rec
}

// TestConfigSecretMemtxShrinkWarns covers the grow-only memtx.memory guard at the
// reconcile layer: memtx.memory cannot shrink at runtime, so when a re-render
// DECREASES it the operator must warn that a restart is required. This is the one
// behavior the config-Secret hash gate uniquely drives (the surrounding
// idempotency is otherwise guaranteed by the deterministic render), and it had no
// test. Asserts both directions — shrink warns, grow/steady does not.
func TestConfigSecretMemtxShrinkWarns(t *testing.T) {
	ctx := context.Background()
	cluster := &v2alpha1.Cluster{}
	cluster.Name, cluster.Namespace = "memtx-cl", "default"

	big := []byte("memtx:\n  memory: 268435456\n")
	small := []byte("memtx:\n  memory: 134217728\n")

	t.Run("shrink warns", func(t *testing.T) {
		r, rec := memtxReconciler(t)
		if err := r.reconcileConfigSecret(ctx, cluster, big, "h1"); err != nil {
			t.Fatal(err)
		}
		if err := r.reconcileConfigSecret(ctx, cluster, small, "h2"); err != nil {
			t.Fatal(err)
		}
		if !recordedReason(rec, "MemtxShrinkRequiresRestart") {
			t.Errorf("expected a MemtxShrinkRequiresRestart warning when memtx.memory decreased")
		}
	})

	t.Run("grow does not warn", func(t *testing.T) {
		r, rec := memtxReconciler(t)
		if err := r.reconcileConfigSecret(ctx, cluster, small, "h1"); err != nil {
			t.Fatal(err)
		}
		if err := r.reconcileConfigSecret(ctx, cluster, big, "h2"); err != nil {
			t.Fatal(err)
		}
		if recordedReason(rec, "MemtxShrinkRequiresRestart") {
			t.Errorf("must NOT warn when memtx.memory grows")
		}
	})
}

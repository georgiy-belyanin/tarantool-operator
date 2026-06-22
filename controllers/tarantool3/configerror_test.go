package tarantool3

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
)

func TestExtractConfigRejection(t *testing.T) {
	cases := []struct {
		name, msg, want string
	}{
		{"cluster_config typo", `2026-... F> [cluster_config] memtx: Unexpected field "memroy"`, `2026-... F> [cluster_config] memtx: Unexpected field "memroy"`},
		// Matches ONLY via the "[cluster_config]" signature: no F> (so not the
		// fatal-config-line path) and none of the other signature phrases — so
		// dropping that signature would regress this case.
		{"cluster_config tag only", `[cluster_config] groups.g.replicasets.r: unknown instance`, `[cluster_config] groups.g.replicasets.r: unknown instance`},
		{"unexpected field alone", `memtx: Unexpected field "memroy"`, `memtx: Unexpected field "memroy"`},
		{"not allowed", `replication.failover "eclection" is not allowed`, `replication.failover "eclection" is not allowed`},
		{"fatal config line", `main F> can't load config: bad`, `main F> can't load config: bad`},
		// A fatal line that is NOT about config must not be misclassified: the
		// fatal-line path requires BOTH "F>" AND "config" (an AND, not an OR).
		{"fatal but not config", `main/103/main F> can't bind to 0.0.0.0:3301`, ""},
		{"buried in a tail", "starting\nloading\n[cluster_config] memtx: Unexpected field \"x\"\nexiting", `[cluster_config] memtx: Unexpected field "x"`},
		{"OOM is not a config error", "Killed\nout of memory", ""},
		{"generic lua traceback", "stack traceback:\n\t[C]: in function 'error'", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractConfigRejection(c.msg); got != c.want {
				t.Errorf("extractConfigRejection() = %q, want %q", got, c.want)
			}
		})
	}
}

// pod builds a single-tarantool-container pod with the given terminated state.
func crashPod(name string, exit int32, lastTerminatedMsg string) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default",
		Labels: map[string]string{ReplicaSetLabel: "rs"}}}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: tarantoolContainerName,
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Message: lastTerminatedMsg},
		},
	}}
	return p
}

func TestPodConfigRejection(t *testing.T) {
	t.Run("config rejection in last-terminated message", func(t *testing.T) {
		p := crashPod("rs-0", 1, `[cluster_config] memtx: Unexpected field "memroy"`)
		if got := podConfigRejection(p); !strings.Contains(got, "memroy") {
			t.Errorf("got %q, want the config reason", got)
		}
	})
	t.Run("clean exit -> no reason", func(t *testing.T) {
		p := crashPod("rs-0", 0, `[cluster_config] memtx: Unexpected field "memroy"`)
		if got := podConfigRejection(p); got != "" {
			t.Errorf("got %q, want empty for a zero exit code", got)
		}
	})
	t.Run("non-config crash -> no reason", func(t *testing.T) {
		p := crashPod("rs-0", 137, "Killed")
		if got := podConfigRejection(p); got != "" {
			t.Errorf("got %q, want empty for an OOM crash", got)
		}
	})
	t.Run("no terminated state -> no reason", func(t *testing.T) {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "rs-0", Namespace: "default"}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: tarantoolContainerName}}
		if got := podConfigRejection(p); got != "" {
			t.Errorf("got %q, want empty when nothing terminated", got)
		}
	})
}

func TestDetectConfigRejection(t *testing.T) {
	rs := &v2alpha1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "default"}}
	healthy := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "rs-0", Namespace: "default",
		Labels: map[string]string{ReplicaSetLabel: "rs"}}}
	bad := crashPod("rs-1", 1, `[cluster_config] memtx: Unexpected field "memroy"`)

	r := &ReplicaSetReconciler{Client: fake.NewClientBuilder().WithObjects(healthy, bad).Build()}
	reason, inst := r.detectConfigRejection(context.Background(), rs)
	if !strings.Contains(reason, "memroy") || inst != "rs-1" {
		t.Errorf("detectConfigRejection() = (%q, %q), want the rejection on rs-1", reason, inst)
	}

	// No rejecting pods -> empty.
	r2 := &ReplicaSetReconciler{Client: fake.NewClientBuilder().WithObjects(healthy).Build()}
	if reason, inst := r2.detectConfigRejection(context.Background(), rs); reason != "" || inst != "" {
		t.Errorf("detectConfigRejection() = (%q, %q), want empty", reason, inst)
	}
}

// TestConfigRejectedSurfacing covers the wiring: a config reason becomes a
// Degraded(ConfigRejected) condition + Error phase via applyStatus, the
// ConfigRejected event is deduped, and a cleared reason restores quorum-based
// Degraded and the normal phase.
func TestConfigRejectedSurfacing(t *testing.T) {
	ctx := context.Background()
	newRS := func() *v2alpha1.ReplicaSet {
		rs := &v2alpha1.ReplicaSet{Spec: v2alpha1.ReplicaSetSpec{Replicas: int32p(1)}}
		rs.Name, rs.Namespace = "rs", "default"
		return rs
	}
	statusClient := func(rs *v2alpha1.ReplicaSet) *ReplicaSetReconciler {
		c := fake.NewClientBuilder().WithScheme(mappingScheme(t)).
			WithStatusSubresource(&v2alpha1.ReplicaSet{}).WithObjects(rs).Build()
		return &ReplicaSetReconciler{Client: c, Recorder: record.NewFakeRecorder(8)}
	}

	t.Run("reason -> Error phase + Degraded ConfigRejected", func(t *testing.T) {
		rs := newRS()
		r := statusClient(rs)
		o := observed{phase: v2alpha1.ReplicaSetError, current: 1, ready: 0,
			configError: `rs-0: [cluster_config] memtx: Unexpected field "memroy"`}
		if err := r.applyStatus(ctx, rs, o); err != nil {
			t.Fatal(err)
		}
		d := meta.FindStatusCondition(rs.Status.Conditions, ConditionDegraded)
		if d == nil || d.Status != metav1.ConditionTrue || d.Reason != "ConfigRejected" || !strings.Contains(d.Message, "memroy") {
			t.Errorf("Degraded = %+v, want True/ConfigRejected with the reason", d)
		}
		if rs.Status.Phase != v2alpha1.ReplicaSetError {
			t.Errorf("phase = %q, want Error", rs.Status.Phase)
		}
	})

	t.Run("cleared reason restores quorum Degraded + non-Error phase", func(t *testing.T) {
		rs := newRS()
		r := statusClient(rs)
		// First a rejection, then recovery (ready, no reason).
		_ = r.applyStatus(ctx, rs, observed{phase: v2alpha1.ReplicaSetError, current: 1, ready: 0,
			configError: "rs-0: bad"})
		_ = r.applyStatus(ctx, rs, observed{phase: v2alpha1.ReplicaSetReady, current: 1, ready: 1})
		d := meta.FindStatusCondition(rs.Status.Conditions, ConditionDegraded)
		if d == nil || d.Status != metav1.ConditionFalse || d.Reason == "ConfigRejected" {
			t.Errorf("Degraded = %+v, want the quorum-based (non-ConfigRejected) False", d)
		}
		if rs.Status.Phase == v2alpha1.ReplicaSetError {
			t.Errorf("phase still Error after recovery")
		}
	})

	t.Run("ConfigRejected event is deduped on a steady reason", func(t *testing.T) {
		rs := newRS()
		rec := record.NewFakeRecorder(8)
		r := &ReplicaSetReconciler{Client: fake.NewClientBuilder().WithScheme(mappingScheme(t)).
			WithStatusSubresource(&v2alpha1.ReplicaSet{}).WithObjects(rs).Build(), Recorder: rec}
		reason := `rs-0: [cluster_config] memtx: Unexpected field "memroy"`
		r.recordConfigRejected(rs, reason)
		// Persist the condition (as runtimeOps->applyStatus would), then repeat.
		_ = r.applyStatus(ctx, rs, observed{phase: v2alpha1.ReplicaSetError, current: 1, ready: 0, configError: reason})
		r.recordConfigRejected(rs, reason) // same reason -> must NOT emit again
		got := 0
		for {
			select {
			case ev := <-rec.Events:
				if strings.Contains(ev, "ConfigRejected") {
					got++
				}
				continue
			default:
			}
			break
		}
		if got != 1 {
			t.Errorf("emitted %d ConfigRejected events, want exactly 1 (deduped)", got)
		}
	})
}

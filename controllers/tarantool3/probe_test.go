package tarantool3

import (
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
)

// probeRS builds a minimal Cluster + ReplicaSet for the probe tests.
func probeRS() (*v2alpha1.Cluster, *v2alpha1.ReplicaSet) {
	cluster := &v2alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "ns1"}}
	rs := &v2alpha1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-001", Namespace: "ns1"},
		Spec: v2alpha1.ReplicaSetSpec{
			ClusterName: "example",
			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "tarantool", Image: "tarantool/tarantool:3"}},
			}},
		},
	}
	return cluster, rs
}

func operatorReadinessProbe(t *testing.T) *corev1.Probe {
	t.Helper()
	cluster, rs := probeRS()
	sts := DesiredStatefulSet(cluster, rs, ConfigSecretName("example"), "h", rs.GetReplicas())
	probe := sts.Spec.Template.Spec.Containers[0].ReadinessProbe
	if probe == nil || probe.Exec == nil {
		t.Fatalf("expected an exec readiness probe, got %+v", probe)
	}
	return probe
}

// TestReadinessProbeAnchoredMatch is the RS3 regression test: the readiness probe
// must match box.info.status with an anchored expression, not a loose substring,
// so an incidental "running" elsewhere in tt's output cannot mark a pod ready.
func TestReadinessProbeAnchoredMatch(t *testing.T) {
	cmd := operatorReadinessProbe(t).Exec.Command
	last := cmd[len(cmd)-1]

	if strings.Contains(last, "grep -q running") {
		t.Errorf("probe uses a loose substring match; want an anchored match: %q", last)
	}
	if !strings.Contains(last, "^- *running$") {
		t.Errorf("probe should anchor on the YAML scalar line '- running', got %q", last)
	}
}

// TestReadinessProbeParameters pins the probe's timing/threshold fields. A probe
// command that reads box.info.status is useless if, say, FailureThreshold is 1
// (a single missed check kills a healthy-but-busy instance) — that mistake leaves
// the command intact, so it must be asserted explicitly.
func TestReadinessProbeParameters(t *testing.T) {
	p := operatorReadinessProbe(t)
	for _, c := range []struct {
		name string
		got  int32
		want int32
	}{
		{"InitialDelaySeconds", p.InitialDelaySeconds, 5},
		{"PeriodSeconds", p.PeriodSeconds, 10},
		{"TimeoutSeconds", p.TimeoutSeconds, 5},
		{"FailureThreshold", p.FailureThreshold, 3},
	} {
		if c.got != c.want {
			t.Errorf("readiness probe %s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// TestReadinessProbeRegexBehavior verifies the anchored expression actually
// accepts/rejects the right box.info.status lines — not merely that some pattern
// is present (a subtly-wrong-but-still-anchored regex would pass a presence
// check). The pattern is extracted from the real probe command so the test can
// never drift from it.
func TestReadinessProbeRegexBehavior(t *testing.T) {
	cmd := operatorReadinessProbe(t).Exec.Command
	last := cmd[len(cmd)-1]

	m := regexp.MustCompile(`grep -qE '([^']*)'`).FindStringSubmatch(last)
	if m == nil {
		t.Fatalf("could not extract the grep -qE pattern from the probe command: %q", last)
	}
	re := regexp.MustCompile(m[1]) // the same ERE the probe greps with

	cases := []struct {
		line  string
		ready bool
	}{
		{"- running", true},      // the box.info.status scalar when ready
		{"-  running", true},     // tolerate extra spacing
		{"- loading", false},     // still coming up
		{"- orphan", false},      // degraded, not ready
		{"running", false},       // unanchored mention must NOT pass
		{"  - running", false},   // not at line start
		{"- running now", false}, // trailing content must NOT pass
	}
	for _, c := range cases {
		if got := re.MatchString(c.line); got != c.ready {
			t.Errorf("probe regex %q on %q = %v, want %v", m[1], c.line, got, c.ready)
		}
	}
}

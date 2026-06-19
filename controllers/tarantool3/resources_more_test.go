package tarantool3

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

func TestConfigSecretName(t *testing.T) {
	if got := ConfigSecretName("example"); got != "example-config" {
		t.Errorf("ConfigSecretName = %q, want example-config", got)
	}
}

func TestTarantoolContainerIndex(t *testing.T) {
	tests := []struct {
		name       string
		containers []corev1.Container
		want       int
	}{
		{"named tarantool wins", []corev1.Container{{Name: "sidecar"}, {Name: "tarantool"}}, 1},
		{"falls back to first", []corev1.Container{{Name: "app"}}, 0},
		{"none", nil, -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tarantoolContainerIndex(tc.containers); got != tc.want {
				t.Errorf("tarantoolContainerIndex = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestInjectConfigIdempotent ensures a re-render does not accumulate duplicate
// volumes, mounts, or env vars on a pod template that already carries them.
func TestInjectConfigIdempotent(t *testing.T) {
	tpl := &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "tarantool"}}},
	}
	injectConfig(tpl, "example-config")
	injectConfig(tpl, "example-config")

	if n := len(tpl.Spec.Volumes); n != 1 {
		t.Errorf("config volume duplicated: %d volumes", n)
	}
	c := tpl.Spec.Containers[0]
	if n := len(c.VolumeMounts); n != 1 {
		t.Errorf("config mount duplicated: %d mounts", n)
	}
	if n := len(c.Env); n != 2 { // TT_CONFIG + TT_INSTANCE_NAME
		t.Errorf("env duplicated: %d env vars, want 2", n)
	}
}

// TestInjectConfigNoContainers must not panic and still adds the volume.
func TestInjectConfigNoContainers(t *testing.T) {
	tpl := &corev1.PodTemplateSpec{}
	injectConfig(tpl, "example-config")
	if len(tpl.Spec.Volumes) != 1 {
		t.Errorf("expected the config volume even without a container: %+v", tpl.Spec.Volumes)
	}
}

func TestUpsertEnvReplacesByName(t *testing.T) {
	env := []corev1.EnvVar{{Name: "TT_CONFIG", Value: "old"}}
	env = upsertEnv(env, corev1.EnvVar{Name: "TT_CONFIG", Value: "new"})
	env = upsertEnv(env, corev1.EnvVar{Name: "OTHER", Value: "x"})
	if len(env) != 2 {
		t.Fatalf("len = %d, want 2", len(env))
	}
	if env[0].Value != "new" {
		t.Errorf("TT_CONFIG = %q, want new (replaced in place)", env[0].Value)
	}
}

func TestMergeLabels(t *testing.T) {
	out := mergeLabels(map[string]string{"a": "1", "b": "base"}, map[string]string{"b": "override", "c": "3"})
	if out["a"] != "1" || out["b"] != "override" || out["c"] != "3" {
		t.Errorf("mergeLabels = %v", out)
	}
}

func TestDesiredStatefulSetRetentionAndHistory(t *testing.T) {
	sts := DesiredStatefulSet(testCluster(), testReplicaSet(), ConfigSecretName("example"), "h", testReplicaSet().GetReplicas())

	if sts.Spec.RevisionHistoryLimit == nil || *sts.Spec.RevisionHistoryLimit != 1 {
		t.Errorf("RevisionHistoryLimit = %v, want 1", sts.Spec.RevisionHistoryLimit)
	}
	rp := sts.Spec.PersistentVolumeClaimRetentionPolicy
	if rp == nil || rp.WhenDeleted != appsv1.RetainPersistentVolumeClaimRetentionPolicyType ||
		rp.WhenScaled != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
		t.Errorf("PVC retention must Retain on delete and scale, got %+v", rp)
	}
}

func TestDesiredStatefulSetVolumeClaimTemplatesPassthrough(t *testing.T) {
	rs := testReplicaSet()
	rs.Spec.VolumeClaimTemplates = []corev1.PersistentVolumeClaim{{
		ObjectMeta: metav1.ObjectMeta{Name: "data"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}}

	sts := DesiredStatefulSet(testCluster(), rs, ConfigSecretName("example"), "h", rs.GetReplicas())
	if len(sts.Spec.VolumeClaimTemplates) != 1 || sts.Spec.VolumeClaimTemplates[0].Name != "data" {
		t.Errorf("volumeClaimTemplates not passed through: %+v", sts.Spec.VolumeClaimTemplates)
	}
}

// TestDesiredStatefulSetInjectsIntoUnnamedContainer covers the fallback path:
// when no container is named "tarantool", the single container still gets the
// config wired in.
func TestDesiredStatefulSetInjectsIntoUnnamedContainer(t *testing.T) {
	rs := testReplicaSet()
	rs.Spec.PodTemplate.Spec.Containers[0].Name = "app"

	sts := DesiredStatefulSet(testCluster(), rs, ConfigSecretName("example"), "h", rs.GetReplicas())
	c := sts.Spec.Template.Spec.Containers[0]

	var hasConfigEnv bool
	for _, e := range c.Env {
		if e.Name == envConfig {
			hasConfigEnv = true
		}
	}
	if !hasConfigEnv {
		t.Errorf("config not injected into the sole (unnamed) container: %+v", c.Env)
	}
}

// TestReadinessProbeUsesAdminSocket pins the probe to the same socket path the
// renderer configures the console on, so the two never drift apart.
func TestReadinessProbeUsesAdminSocket(t *testing.T) {
	sts := DesiredStatefulSet(testCluster(), testReplicaSet(), ConfigSecretName("example"), "h", testReplicaSet().GetReplicas())
	probe := sts.Spec.Template.Spec.Containers[0].ReadinessProbe
	if probe == nil || probe.Exec == nil {
		t.Fatalf("expected exec readiness probe")
	}
	cmd := probe.Exec.Command[len(probe.Exec.Command)-1]
	if !strings.Contains(cmd, clusterconfig.AdminSocketPath) {
		t.Errorf("readiness probe %q does not reference admin socket %q", cmd, clusterconfig.AdminSocketPath)
	}
}

func TestDesiredPodDisruptionBudget(t *testing.T) {
	rs := testReplicaSet() // replicas = 2
	pdb := DesiredPodDisruptionBudget(testCluster(), rs)

	if pdb.Name != PDBName("storage-001") || pdb.Name != "storage-001-pdb" {
		t.Errorf("PDB name = %q, want storage-001-pdb", pdb.Name)
	}
	if pdb.Spec.MaxUnavailable == nil || pdb.Spec.MaxUnavailable.IntValue() != 1 {
		t.Errorf("MaxUnavailable = %v, want 1", pdb.Spec.MaxUnavailable)
	}
	if pdb.Spec.MinAvailable != nil {
		t.Errorf("MinAvailable should be unset (using MaxUnavailable), got %v", pdb.Spec.MinAvailable)
	}
	if pdb.Spec.Selector == nil || pdb.Spec.Selector.MatchLabels[ReplicaSetLabel] != "storage-001" {
		t.Errorf("PDB selector must match the replicaset's pods: %+v", pdb.Spec.Selector)
	}
}

func TestInjectLeaderStepDown(t *testing.T) {
	t.Run("election gets a demote preStop", func(t *testing.T) {
		sts := DesiredStatefulSet(testCluster(), testReplicaSet(), ConfigSecretName("example"), "h", testReplicaSet().GetReplicas())
		injectLeaderStepDown(&sts.Spec.Template, "election")
		lc := sts.Spec.Template.Spec.Containers[0].Lifecycle
		if lc == nil || lc.PreStop == nil || lc.PreStop.Exec == nil {
			t.Fatalf("expected a preStop exec hook for election, got %+v", lc)
		}
		cmd := lc.PreStop.Exec.Command[len(lc.PreStop.Exec.Command)-1]
		if !strings.Contains(cmd, "box.ctl.demote") || !strings.Contains(cmd, clusterconfig.AdminSocketPath) {
			t.Errorf("preStop should demote via the admin socket, got %q", cmd)
		}
	})

	for _, f := range []string{"off", "manual", "supervised"} {
		t.Run("no preStop for "+f, func(t *testing.T) {
			sts := DesiredStatefulSet(testCluster(), testReplicaSet(), ConfigSecretName("example"), "h", testReplicaSet().GetReplicas())
			injectLeaderStepDown(&sts.Spec.Template, f)
			if lc := sts.Spec.Template.Spec.Containers[0].Lifecycle; lc != nil && lc.PreStop != nil {
				t.Errorf("%s must not set a demote preStop (no auto-recovery): %+v", f, lc.PreStop)
			}
		})
	}

	t.Run("user preStop preserved", func(t *testing.T) {
		rs := testReplicaSet()
		custom := &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"mine"}}}}
		rs.Spec.PodTemplate.Spec.Containers[0].Lifecycle = custom
		sts := DesiredStatefulSet(testCluster(), rs, ConfigSecretName("example"), "h", rs.GetReplicas())
		injectLeaderStepDown(&sts.Spec.Template, "election")
		got := sts.Spec.Template.Spec.Containers[0].Lifecycle.PreStop.Exec.Command
		if len(got) != 1 || got[0] != "mine" {
			t.Errorf("user preStop must be preserved, got %v", got)
		}
	})
}

package tarantool3

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
)

func int32p(v int32) *int32 { return &v }

func testCluster() *v2alpha1.Cluster {
	return &v2alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "tarantool"},
		Spec:       v2alpha1.ClusterSpec{Domain: "cluster.local"},
	}
}

func testReplicaSet() *v2alpha1.ReplicaSet {
	return &v2alpha1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "storage-001", Namespace: "tarantool"},
		Spec: v2alpha1.ReplicaSetSpec{
			ClusterName: "example",
			Group:       "storages",
			Replicas:    int32p(2),
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "tarantool",
						Image: "tarantool/tarantool:3",
					}},
				},
			},
		},
	}
}

func TestDesiredHeadlessService(t *testing.T) {
	svc := DesiredHeadlessService(testCluster())

	if svc.Name != "example" || svc.Namespace != "tarantool" {
		t.Fatalf("unexpected name/ns: %s/%s", svc.Namespace, svc.Name)
	}
	if svc.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Errorf("service must be headless, got ClusterIP=%q", svc.Spec.ClusterIP)
	}
	if !svc.Spec.PublishNotReadyAddresses {
		t.Errorf("PublishNotReadyAddresses must be true for cluster formation")
	}
	if got := svc.Spec.Selector[ClusterLabel]; got != "example" {
		t.Errorf("selector %s = %q, want example", ClusterLabel, got)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != 3301 {
		t.Errorf("unexpected ports: %+v", svc.Spec.Ports)
	}
}

func TestDesiredStatefulSet(t *testing.T) {
	sts := DesiredStatefulSet(testCluster(), testReplicaSet(), ConfigSecretName("example"), "deadbeef", testReplicaSet().GetReplicas())

	if sts.Name != "storage-001" {
		t.Fatalf("name = %q, want storage-001", sts.Name)
	}
	if sts.Spec.ServiceName != "example" {
		t.Errorf("ServiceName = %q, want example (headless service)", sts.Spec.ServiceName)
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 2 {
		t.Errorf("Replicas = %v, want 2", sts.Spec.Replicas)
	}
	if sts.Spec.PodManagementPolicy != "Parallel" {
		t.Errorf("PodManagementPolicy = %q, want Parallel", sts.Spec.PodManagementPolicy)
	}
	if got := sts.Spec.Template.Annotations[ConfigHashAnnotation]; got != "deadbeef" {
		t.Errorf("config-hash annotation = %q, want deadbeef", got)
	}
	if got := sts.Spec.Selector.MatchLabels[ReplicaSetLabel]; got != "storage-001" {
		t.Errorf("selector %s = %q, want storage-001", ReplicaSetLabel, got)
	}

	// The config Secret must be mounted and the container pointed at it.
	var hasVolume bool
	for _, v := range sts.Spec.Template.Spec.Volumes {
		if v.Name == configVolumeName && v.Secret != nil && v.Secret.SecretName == ConfigSecretName("example") {
			hasVolume = true
		}
	}
	if !hasVolume {
		t.Fatalf("config secret volume not injected: %+v", sts.Spec.Template.Spec.Volumes)
	}

	c := sts.Spec.Template.Spec.Containers[0]
	var hasMount bool
	for _, m := range c.VolumeMounts {
		if m.Name == configVolumeName && m.MountPath == configMountPath {
			hasMount = true
		}
	}
	if !hasMount {
		t.Errorf("config volume mount not injected: %+v", c.VolumeMounts)
	}

	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	if env[envConfig].Value != configMountPath+"/"+ConfigFileName {
		t.Errorf("%s = %q, want %s", envConfig, env[envConfig].Value, configMountPath+"/"+ConfigFileName)
	}
	if ref := env[envInstanceName].ValueFrom; ref == nil || ref.FieldRef == nil || ref.FieldRef.FieldPath != "metadata.name" {
		t.Errorf("%s must come from metadata.name, got %+v", envInstanceName, env[envInstanceName])
	}

	// Readiness probe queries box.info via the admin console socket.
	if c.ReadinessProbe == nil || c.ReadinessProbe.Exec == nil {
		t.Fatalf("expected an exec readiness probe, got %+v", c.ReadinessProbe)
	}
	probeCmd := c.ReadinessProbe.Exec.Command[len(c.ReadinessProbe.Exec.Command)-1]
	if !strings.Contains(probeCmd, "box.info.status") || !strings.Contains(probeCmd, "tt connect") {
		t.Errorf("readiness probe should query box.info.status via tt connect, got %q", probeCmd)
	}
}

func TestDesiredStatefulSetRespectsUserReadinessProbe(t *testing.T) {
	rs := testReplicaSet()
	custom := &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{}}}
	rs.Spec.PodTemplate.Spec.Containers[0].ReadinessProbe = custom

	sts := DesiredStatefulSet(testCluster(), rs, ConfigSecretName("example"), "h", rs.GetReplicas())
	if p := sts.Spec.Template.Spec.Containers[0].ReadinessProbe; p == nil || p.Exec != nil {
		t.Errorf("user-provided readiness probe must be preserved, got %+v", p)
	}
}

// TestDesiredStatefulSetDoesNotMutateInput guards the DeepCopy of the pod
// template: injecting config must not mutate the ReplicaSet spec.
func TestDesiredStatefulSetDoesNotMutateInput(t *testing.T) {
	rs := testReplicaSet()
	_ = DesiredStatefulSet(testCluster(), rs, ConfigSecretName("example"), "hash", rs.GetReplicas())

	if n := len(rs.Spec.PodTemplate.Spec.Volumes); n != 0 {
		t.Errorf("input pod template was mutated: %d volumes added", n)
	}
	if n := len(rs.Spec.PodTemplate.Spec.Containers[0].Env); n != 0 {
		t.Errorf("input pod template container was mutated: %d env vars added", n)
	}
}

// TestDesiredStatefulSetLabelsAndSelector checks the identifying labels are
// stamped on the StatefulSet, its pod template, and the selector consistently.
func TestDesiredStatefulSetLabelsAndSelector(t *testing.T) {
	sts := DesiredStatefulSet(testCluster(), testReplicaSet(), ConfigSecretName("example"), "h", testReplicaSet().GetReplicas())

	for _, labels := range []map[string]string{sts.Labels, sts.Spec.Template.Labels, sts.Spec.Selector.MatchLabels} {
		if labels[ClusterLabel] != "example" {
			t.Errorf("%s = %q, want example", ClusterLabel, labels[ClusterLabel])
		}
		if labels[ReplicaSetLabel] != "storage-001" {
			t.Errorf("%s = %q, want storage-001", ReplicaSetLabel, labels[ReplicaSetLabel])
		}
		if labels[ManagedByLabel] != ManagedByValue {
			t.Errorf("%s = %q, want %s", ManagedByLabel, labels[ManagedByLabel], ManagedByValue)
		}
	}
	if sts.Spec.Template.Labels[GroupLabel] != "storages" {
		t.Errorf("pod template %s = %q, want storages", GroupLabel, sts.Spec.Template.Labels[GroupLabel])
	}
}

// TestDesiredStatefulSetPreservesUserPodLabels: operator labels are merged on
// top of, not in place of, labels the user set on the pod template.
func TestDesiredStatefulSetPreservesUserPodLabels(t *testing.T) {
	rs := testReplicaSet()
	rs.Spec.PodTemplate.Labels = map[string]string{"team": "db"}

	sts := DesiredStatefulSet(testCluster(), rs, ConfigSecretName("example"), "h", rs.GetReplicas())
	if sts.Spec.Template.Labels["team"] != "db" {
		t.Errorf("user pod label dropped: %v", sts.Spec.Template.Labels)
	}
	if sts.Spec.Template.Labels[ClusterLabel] != "example" {
		t.Errorf("operator label missing after merge: %v", sts.Spec.Template.Labels)
	}
}

// TestDesiredStatefulSetPassesScheduleFields: MinReadySeconds and UpdateStrategy
// from the spec flow through to the StatefulSet.
func TestDesiredStatefulSetPassesScheduleFields(t *testing.T) {
	rs := testReplicaSet()
	rs.Spec.MinReadySeconds = 7
	rs.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}

	sts := DesiredStatefulSet(testCluster(), rs, ConfigSecretName("example"), "h", rs.GetReplicas())
	if sts.Spec.MinReadySeconds != 7 {
		t.Errorf("MinReadySeconds = %d, want 7", sts.Spec.MinReadySeconds)
	}
	if sts.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType {
		t.Errorf("UpdateStrategy.Type = %q, want OnDelete", sts.Spec.UpdateStrategy.Type)
	}
}

func TestDesiredHeadlessServiceManagedByLabel(t *testing.T) {
	svc := DesiredHeadlessService(testCluster())
	if svc.Labels[ManagedByLabel] != ManagedByValue {
		t.Errorf("service %s = %q, want %s", ManagedByLabel, svc.Labels[ManagedByLabel], ManagedByValue)
	}
}

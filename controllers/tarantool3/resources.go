// Package tarantool3 contains the controller-runtime reconcilers for the
// Tarantool 3 (Cartridge-free) db.tarantool.io/v2alpha1 API.
//
// These are fresh reconcilers, not the legacy Cartridge stepped-reconciler
// framework: a Cluster renders its declarative config into a Secret and owns a
// headless Service; each ReplicaSet renders into one StatefulSet whose pods are
// the Tarantool 3 instances. See AGENTS.md.
package tarantool3

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// Labels and annotations the operator manages.
const (
	ManagedByLabel  = "app.kubernetes.io/managed-by"
	ManagedByValue  = "tarantool-operator"
	ClusterLabel    = "tarantool.io/cluster"
	GroupLabel      = "tarantool.io/group"
	ReplicaSetLabel = "tarantool.io/replicaset"

	// ConfigHashAnnotation carries the clusterconfig.Hash of the delivered
	// config. It lives on the config Secret and is copied onto the instance pod
	// template so a config change rolls the pods.
	ConfigHashAnnotation = "tarantool.io/config-hash"
)

// Conventions for delivering the rendered config into pods. The rendered config
// is mounted from a Secret (it contains credentials) and the Tarantool 3 process
// is pointed at it via the TT_CONFIG / TT_INSTANCE_NAME environment variables.
const (
	configVolumeName       = "tarantool-config"
	configMountPath        = "/etc/tarantool"
	ConfigFileName         = "config.yaml"
	tarantoolContainerName = "tarantool"

	envConfig       = "TT_CONFIG"
	envInstanceName = "TT_INSTANCE_NAME"
)

// ConfigSecretName is the name of the Secret holding a cluster's rendered config.
func ConfigSecretName(clusterName string) string {
	return clusterName + "-config"
}

// OperatorUserSecretName is the name of the Secret holding the generated
// password of the operator's built-in Tarantool user (data key = user name,
// same convention as credentialsSecret).
func OperatorUserSecretName(clusterName string) string {
	return clusterName + "-operator-user"
}

func clusterLabels(clusterName string) map[string]string {
	return map[string]string{
		ClusterLabel:   clusterName,
		ManagedByLabel: ManagedByValue,
	}
}

func replicaSetLabels(clusterName, group, rsName string) map[string]string {
	return map[string]string{
		ClusterLabel:    clusterName,
		GroupLabel:      group,
		ReplicaSetLabel: rsName,
		ManagedByLabel:  ManagedByValue,
	}
}

// DesiredHeadlessService builds the headless Service that gives every instance
// pod a stable DNS name (<pod>.<cluster>.<ns>.svc.<domain>), matching the
// advertise URIs the renderer emits. PublishNotReadyAddresses is set so peers
// resolve each other during initial cluster formation, before they are ready.
func DesiredHeadlessService(cluster *v2alpha1.Cluster) *corev1.Service {
	port := clusterconfig.ClusterPort(cluster)

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
			Labels:    clusterLabels(cluster.Name),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 map[string]string{ClusterLabel: cluster.Name},
			Ports: []corev1.ServicePort{{
				Name:       "iproto",
				Port:       port,
				TargetPort: intstr.FromInt(int(port)),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}

// DesiredStatefulSet builds the StatefulSet for one ReplicaSet. Pod names equal
// the replicaset's instance names (<rs>-<ordinal>), so they line up with the
// rendered config and the advertise URIs. The rendered config Secret is mounted
// into the tarantool container and selected via TT_CONFIG/TT_INSTANCE_NAME.
// replicas is normally rs.GetReplicas(); the scale-up gate passes the current
// (smaller) StatefulSet size while config delivery to the existing instances
// is still in flight.
func DesiredStatefulSet(cluster *v2alpha1.Cluster, rs *v2alpha1.ReplicaSet, configSecretName, configHash string, replicas int32) *appsv1.StatefulSet {
	labels := replicaSetLabels(cluster.Name, rs.GetGroup(), rs.Name)
	revisionHistoryLimit := int32(1)

	template := rs.Spec.PodTemplate.DeepCopy()
	template.Labels = mergeLabels(template.Labels, labels)
	if template.Annotations == nil {
		template.Annotations = map[string]string{}
	}
	template.Annotations[ConfigHashAnnotation] = configHash
	injectConfig(template, configSecretName)

	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rs.Name,
			Namespace: rs.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:             &replicas,
			ServiceName:          cluster.Name,
			PodManagementPolicy:  appsv1.ParallelPodManagement,
			RevisionHistoryLimit: &revisionHistoryLimit,
			Selector:             &metav1.LabelSelector{MatchLabels: labels},
			Template:             *template,
			VolumeClaimTemplates: rs.Spec.VolumeClaimTemplates,
			UpdateStrategy:       rs.Spec.UpdateStrategy,
			MinReadySeconds:      rs.Spec.MinReadySeconds,
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
		},
	}
}

// PDBName is the name of the PodDisruptionBudget for a replica set's instances.
func PDBName(rsName string) string { return rsName + "-pdb" }

// DesiredPodDisruptionBudget builds the PodDisruptionBudget that protects a
// replica set's instances during voluntary disruptions (node drains, cluster
// upgrades). maxUnavailable=1 lets a drain take at most one instance down at a
// time, which preserves quorum for an election replica set of 3+ instances and is
// the conservative default for the others. Only meaningful for multi-instance sets;
// the reconciler skips it for a single instance (nothing to protect, and it must
// not block draining the lone node).
func DesiredPodDisruptionBudget(cluster *v2alpha1.Cluster, rs *v2alpha1.ReplicaSet) *policyv1.PodDisruptionBudget {
	labels := replicaSetLabels(cluster.Name, rs.GetGroup(), rs.Name)
	maxUnavailable := intstr.FromInt(1)
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PDBName(rs.Name),
			Namespace: rs.Namespace,
			Labels:    labels,
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       &metav1.LabelSelector{MatchLabels: labels},
		},
	}
}

// injectConfig mounts the rendered config Secret into the tarantool container
// and wires TT_CONFIG / TT_INSTANCE_NAME so the process loads its config and
// knows which instance it is. Assumes the image entrypoint honors these env
// vars (verified in the kind e2e); does nothing if no container is found.
func injectConfig(t *corev1.PodTemplateSpec, secretName string) {
	t.Spec.Volumes = upsertVolume(t.Spec.Volumes, corev1.Volume{
		Name: configVolumeName,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: secretName},
		},
	})

	idx := tarantoolContainerIndex(t.Spec.Containers)
	if idx < 0 {
		return
	}
	c := &t.Spec.Containers[idx]

	// Capture the crash-log tail in the pod's terminated state so the operator
	// can read a config rejection (Tarantool exits before iproto is up, so the
	// reason is only in its log) from pod status — no pods/log RBAC needed. Only
	// when the user left the policy unset.
	if c.TerminationMessagePolicy == "" {
		c.TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError
	}

	c.VolumeMounts = upsertVolumeMount(c.VolumeMounts, corev1.VolumeMount{
		Name:      configVolumeName,
		MountPath: configMountPath,
		ReadOnly:  true,
	})
	c.Env = upsertEnv(c.Env, corev1.EnvVar{
		Name:  envConfig,
		Value: configMountPath + "/" + ConfigFileName,
	})
	c.Env = upsertEnv(c.Env, corev1.EnvVar{
		Name: envInstanceName,
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
		},
	})

	// Readiness via the admin console socket: ready only once box.info.status is
	// "running" (a TCP check would pass while the instance is still LOADING).
	// Respect a probe the user already set on the container.
	if c.ReadinessProbe == nil {
		c.ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{
					"sh", "-c",
					// Anchored match on the YAML scalar line ("- running") so an
					// incidental "running" elsewhere in tt's output can't pass.
					fmt.Sprintf("echo 'return box.info.status' | tt connect %s 2>/dev/null | grep -qE '^- *running$'", clusterconfig.AdminSocketPath),
				}},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
			TimeoutSeconds:      5,
			FailureThreshold:    3,
		}
	}
}

// injectLeaderStepDown adds a preStop hook that asks the instance to step down as
// leader before it is terminated, so the replica set re-elects immediately instead
// of waiting for failure detection — shrinking the write-unavailability window on a
// rollout or node drain. Only for election failover (the mode with automatic
// recovery): in off/manual/supervised, demoting the writable instance on shutdown
// would drop the set to read-only with no auto-recovery, so we leave shutdown to
// Tarantool's native SIGTERM drain. box.ctl.demote() is a harmless no-op on a
// follower; the hook is wrapped so it can never fail or stall the termination, and
// a user-provided preStop is preserved.
func injectLeaderStepDown(t *corev1.PodTemplateSpec, failover string) {
	if failover != clusterconfig.FailoverElection {
		return
	}
	idx := tarantoolContainerIndex(t.Spec.Containers)
	if idx < 0 {
		return
	}
	c := &t.Spec.Containers[idx]
	if c.Lifecycle != nil && c.Lifecycle.PreStop != nil {
		return // respect a user-provided preStop hook
	}
	if c.Lifecycle == nil {
		c.Lifecycle = &corev1.Lifecycle{}
	}
	c.Lifecycle.PreStop = &corev1.LifecycleHandler{
		Exec: &corev1.ExecAction{Command: []string{
			"sh", "-c",
			fmt.Sprintf("echo 'pcall(function() box.ctl.demote() end)' | timeout 10 tt connect %s 2>/dev/null || true", clusterconfig.AdminSocketPath),
		}},
	}
}

// tarantoolContainerIndex returns the index of the container named "tarantool",
// or 0 if there is at least one container, or -1 when there are none.
func tarantoolContainerIndex(containers []corev1.Container) int {
	for i := range containers {
		if containers[i].Name == tarantoolContainerName {
			return i
		}
	}
	if len(containers) > 0 {
		return 0
	}
	return -1
}

func mergeLabels(base, extra map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// upsert{Volume,VolumeMount,Env} replace the entry with the same Name, or append
// it — so re-injecting is idempotent and never clobbers a user-provided entry of
// a different name. Three small look-alikes beat a generic: Go has no way to read
// a `.Name` field through a type parameter, so a generic version needs a per-type
// accessor closure anyway, which is more apparatus than the duplication it saves.

func upsertVolume(vols []corev1.Volume, v corev1.Volume) []corev1.Volume {
	for i := range vols {
		if vols[i].Name == v.Name {
			vols[i] = v
			return vols
		}
	}
	return append(vols, v)
}

func upsertVolumeMount(mounts []corev1.VolumeMount, m corev1.VolumeMount) []corev1.VolumeMount {
	for i := range mounts {
		if mounts[i].Name == m.Name {
			mounts[i] = m
			return mounts
		}
	}
	return append(mounts, m)
}

func upsertEnv(env []corev1.EnvVar, e corev1.EnvVar) []corev1.EnvVar {
	for i := range env {
		if env[i].Name == e.Name {
			env[i] = e
			return env
		}
	}
	return append(env, e)
}

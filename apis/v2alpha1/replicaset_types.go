package v2alpha1

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DefaultGroup is the Tarantool 3 configuration group a ReplicaSet is placed in
// when spec.group is unset.
const DefaultGroup = "default"

// ReplicaSetSpec defines the desired state of a single Tarantool 3 replicaset.
//
// One ReplicaSet maps 1:1 to one Tarantool 3 replicaset, materialized as one
// StatefulSet whose pods are the replicaset's instances. A sharded storage tier
// is expressed as several ReplicaSet objects (the operator does not stamp N
// replicasets from a single object, unlike the legacy Cartridge Role).
type ReplicaSetSpec struct {
	// ClusterName references the owning Cluster in the same namespace. Its
	// cluster-wide settings (credentials, failover, sharding, domain) are merged
	// above this replicaset's scope when the configuration is rendered.
	// Immutable: it is baked into the StatefulSet's (immutable) pod selector and
	// the rendered configuration topology — changing it would orphan the running
	// StatefulSet and brick the reconcile on a selector update.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterName is immutable"
	ClusterName string `json:"clusterName"`

	// Group is the Tarantool 3 configuration group this replicaset belongs to.
	// Groups separate, for example, storage replicasets from router replicasets.
	// Defaults to "default". Immutable for the same reason as clusterName: the
	// group is part of the StatefulSet's immutable pod selector.
	// +optional
	// +kubebuilder:default=default
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="group is immutable"
	Group string `json:"group,omitempty"`

	// Replicas is the number of instances in the replicaset, defaults to 1. One
	// instance is the leader (writable) and the rest are read-only replicas,
	// subject to the cluster failover mode.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Config is arbitrary Tarantool 3 configuration merged at this replicaset's
	// scope. Use it for keys the operator does not model explicitly. Cluster-scope
	// and operator-managed keys take precedence on conflict.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Config *apiextensionsv1.JSON `json:"config,omitempty"`

	// InstanceConfigs is raw Tarantool 3 configuration merged at single-instance
	// scope, keyed by the instance ordinal ("0", "1", ...). Keys beyond
	// replicas-1 are ignored. Instance scope has the highest precedence; arrays
	// REPLACE lower-scope arrays rather than appending.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	InstanceConfigs map[string]apiextensionsv1.JSON `json:"instanceConfigs,omitempty"`

	// PodTemplate describes the Tarantool instance pods. The tarantool container
	// image, resources, volume mounts and env are taken from here.
	// +kubebuilder:validation:Required
	PodTemplate v1.PodTemplateSpec `json:"podTemplate"`

	// VolumeClaimTemplates is a list of claims that the instance pods may
	// reference for persistent storage (snapshots and WAL).
	// +optional
	VolumeClaimTemplates []v1.PersistentVolumeClaim `json:"volumeClaimTemplates,omitempty"`

	// UpdateStrategy controls how the underlying StatefulSet rolls out pod and
	// configuration changes. When left unset and the cluster uses election or
	// supervised failover (with a super-role user available), the operator
	// manages rollouts itself: pods restart one at a time, followers first and
	// the current leader last, so a rollout costs exactly one leadership
	// transition. Setting any explicit strategy here disables that and passes
	// the strategy through to the StatefulSet verbatim.
	// +optional
	UpdateStrategy appsv1.StatefulSetUpdateStrategy `json:"updateStrategy,omitempty"`

	// MinReadySeconds is the minimum number of seconds for which a newly created
	// instance pod must be ready, without any container crashing, to be considered
	// available. Defaults to 0.
	// +optional
	// +kubebuilder:default=0
	MinReadySeconds int32 `json:"minReadySeconds,omitempty"`
}

// ReplicaSetPhase is a coarse, human-readable label for a ReplicaSet's condition.
// +enum
type ReplicaSetPhase string

const (
	ReplicaSetPending     ReplicaSetPhase = "Pending"
	ReplicaSetConfiguring ReplicaSetPhase = "Configuring"
	ReplicaSetReady       ReplicaSetPhase = "Ready"
	// ReplicaSetDegraded: the set had been fully ready at least once and has
	// since lost instances — previously achieved health regressed (a stuck or
	// dead instance), as opposed to Configuring, the initial convergence.
	ReplicaSetDegraded ReplicaSetPhase = "Degraded"
	ReplicaSetError    ReplicaSetPhase = "Error"
)

// ReplicaSetStatus defines the observed state of a ReplicaSet.
type ReplicaSetStatus struct {
	// Phase indicates the current high-level state of the replicaset.
	// +optional
	// +kubebuilder:default=Pending
	Phase ReplicaSetPhase `json:"phase,omitempty"`

	// ReadyInstances is a string in the form "ready/total" for display.
	// +optional
	ReadyInstances string `json:"readyInstances,omitempty"`

	// Bootstrapped is a sticky flag set true once every instance of the replica
	// set has become ready at least once. It gates runtime-only behavior that is
	// unsafe during the initial bootstrap race — notably replication.autoexpel,
	// which can expel peers while a fresh multi-instance set is still forming. Once
	// true it is never reset, so the gated behavior does not flap on later restarts.
	// +optional
	Bootstrapped bool `json:"bootstrapped,omitempty"`

	// SchemaUpgradedForImage records the tarantool container image for which the
	// operator last ran box.schema.upgrade() on the replica set's leader (after a
	// completed rollout with homogeneous binary versions). When the image changes,
	// the operator re-runs the schema upgrade once the new rollout converges.
	// +optional
	SchemaUpgradedForImage string `json:"schemaUpgradedForImage,omitempty"`

	// ShardingBootstrapped is a sticky flag set true once the operator has
	// bootstrapped vshard through this replica set's router (distributing the
	// buckets across the storages). Only meaningful for a router replica set; once
	// true the operator stops re-issuing the idempotent bootstrap call.
	// +optional
	ShardingBootstrapped bool `json:"shardingBootstrapped,omitempty"`

	// Replicas is the current number of instance pods, exposed via the scale
	// subresource so `kubectl scale` and HorizontalPodAutoscaler can target the
	// ReplicaSet. (Autoscaling a quorum database is discouraged; the subresource is
	// provided mainly for `kubectl scale`.)
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Selector is the serialized label selector for this replica set's instance
	// pods, used by the scale subresource (and HPA) to find them.
	// +optional
	Selector string `json:"selector,omitempty"`

	// Leader is the instance currently acting as the writable leader of this
	// replicaset, as reported by box.info.
	// +optional
	Leader string `json:"leader,omitempty"`

	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the detailed condition set for the replicaset.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ReplicaSet is the Schema for the Tarantool 3 replicasets API. Each ReplicaSet
// is one replicaset within a Cluster and is materialized as one StatefulSet.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:storageversion
// +kubebuilder:resource:shortName=ttrs
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.clusterName",priority=0
// +kubebuilder:printcolumn:name="Group",type="string",JSONPath=".spec.group",priority=1
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",priority=0
// +kubebuilder:printcolumn:name="Instances",type="string",JSONPath=".status.readyInstances",priority=0
// +kubebuilder:printcolumn:name="Leader",type="string",JSONPath=".status.leader",priority=1
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type ReplicaSet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReplicaSetSpec   `json:"spec,omitempty"`
	Status ReplicaSetStatus `json:"status,omitempty"`
}

// GetGroup returns the configured group or the default.
func (in *ReplicaSet) GetGroup() string {
	if in.Spec.Group == "" {
		return DefaultGroup
	}

	return in.Spec.Group
}

// GetReplicas returns the desired instance count, defaulting to 1.
func (in *ReplicaSet) GetReplicas() int32 {
	if in.Spec.Replicas == nil {
		return 1
	}

	return *in.Spec.Replicas
}

// InstanceName returns the name of the ordinal-th instance of this replicaset,
// which matches the corresponding StatefulSet pod name.
func (in *ReplicaSet) InstanceName(ordinal int32) string {
	return fmt.Sprintf("%s-%d", in.Name, ordinal)
}

func (in *ReplicaSet) SetReadyInstances(ready int32) {
	in.Status.ReadyInstances = fmt.Sprintf("%d/%d", ready, in.GetReplicas())
}

// ReplicaSetList contains a list of ReplicaSet.
// +kubebuilder:object:root=true
type ReplicaSetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReplicaSet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ReplicaSet{}, &ReplicaSetList{})
}

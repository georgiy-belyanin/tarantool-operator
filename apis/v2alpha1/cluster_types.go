package v2alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// DefaultDomain is the Kubernetes cluster DNS domain assumed when a Cluster
	// does not set spec.domain.
	DefaultDomain = "cluster.local"
)

// ClusterSpec defines the cluster-wide Tarantool 3 configuration shared by every
// ReplicaSet that belongs to this Cluster. It maps onto the global scope of the
// Tarantool 3 declarative configuration (credentials, iproto, replication,
// sharding). Per-replicaset and per-instance settings live on ReplicaSet.
type ClusterSpec struct {
	// Domain is the Kubernetes cluster DNS domain used to build stable instance
	// advertise URIs, defaults to "cluster.local".
	// +optional
	// +kubebuilder:default=cluster.local
	Domain string `json:"domain,omitempty"`

	// CredentialsSecret names a Secret (in the Cluster's namespace) holding
	// Tarantool user passwords: each data key is a user name declared under
	// credentials.users in the config, the value its password. The operator
	// injects each password into the rendered config for matching users that have
	// none inline, so passwords never appear in the CR. Users/roles themselves are
	// plain Tarantool config (config.credentials).
	// +optional
	CredentialsSecret string `json:"credentialsSecret,omitempty"`

	// GroupConfigs is raw Tarantool 3 configuration merged at each named group's
	// scope (groups.<name>). Keys for groups that no ReplicaSet belongs to are
	// ignored. Note Tarantool merge semantics across scopes: maps deep-merge,
	// arrays REPLACE (an instance-scope array fully overrides a group-scope one).
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	GroupConfigs map[string]apiextensionsv1.JSON `json:"groupConfigs,omitempty"`

	// Config is arbitrary Tarantool 3 configuration merged into the global scope
	// of the rendered cluster config. Use it for keys the operator does not model
	// explicitly (e.g. log, memtx, app). Operator-managed keys take precedence on
	// conflict.
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Config *apiextensionsv1.JSON `json:"config,omitempty"`

	// DisableOperatorUser turns off the operator's built-in privileged user.
	// By default the operator declares a "tarantool-operator" user with the
	// super role and a generated password (kept in the <cluster>-operator-user
	// Secret), so its runtime features — leader observation, configuration hot
	// reload, schema upgrade after image rollouts, drain-gated scale-in — work
	// without any manual credentials setup. When disabled, those features key
	// off the first user with the super role from the cluster credentials, or
	// degrade gracefully when there is none. A user-declared
	// credentials.users."tarantool-operator" always takes precedence over the
	// built-in definition.
	// +optional
	DisableOperatorUser bool `json:"disableOperatorUser,omitempty"`
}

// ClusterPhase is a coarse, human-readable label for a Cluster's condition.
// +enum
type ClusterPhase string

const (
	ClusterPending     ClusterPhase = "Pending"
	ClusterConfiguring ClusterPhase = "Configuring"
	ClusterReady       ClusterPhase = "Ready"
	ClusterError       ClusterPhase = "Error"
)

// ClusterStatus defines the observed state of a Cluster.
type ClusterStatus struct {
	// Phase indicates the current high-level state of the cluster.
	// +optional
	// +kubebuilder:default=Pending
	Phase ClusterPhase `json:"phase,omitempty"`

	// ObservedGeneration is the .metadata.generation last reconciled by the
	// operator.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions holds the detailed condition set for the cluster.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// Cluster is the Schema for the Tarantool 3 clusters API. It owns the
// cluster-wide configuration; member instances are described by ReplicaSet
// resources that reference this Cluster by name.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:storageversion
// +kubebuilder:resource:shortName=ttc
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",priority=0
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type Cluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterSpec   `json:"spec,omitempty"`
	Status ClusterStatus `json:"status,omitempty"`
}

// GetDomain returns the configured DNS domain or the default.
func (in *Cluster) GetDomain() string {
	if in.Spec.Domain == "" {
		return DefaultDomain
	}

	return in.Spec.Domain
}

// ClusterList contains a list of Cluster.
// +kubebuilder:object:root=true
type ClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Cluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Cluster{}, &ClusterList{})
}

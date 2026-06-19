// Package v2alpha1 contains the API Schema definitions for the Tarantool 3
// native (Cartridge-free) operator API. It lives in its own API group,
// db.tarantool.io, so it is a distinct set of CRDs from the legacy Cartridge
// API (tarantool.io/v1beta1) and the two can coexist without a conversion
// webhook. See AGENTS.md for the full migration plan.
//
// +kubebuilder:object:generate=true
// +groupName=db.tarantool.io
package v2alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "db.tarantool.io", Version: "v2alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

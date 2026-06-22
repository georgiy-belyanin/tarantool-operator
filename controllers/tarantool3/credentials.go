package tarantool3

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
)

// PasswordSource resolves Tarantool user passwords for a cluster from somewhere
// other than the in-namespace credentialsSecret — e.g. HashiCorp Vault. It is an
// extension seam: optional sources register themselves with registerPasswordSource
// from a build-tagged file, so a build that excludes the tag contains no trace of
// them (no code, no dependency). The base credentialsSecret path always works.
type PasswordSource interface {
	// Name identifies the source in errors and logs.
	Name() string
	// Passwords returns user->password for the cluster. A nil/empty map with a nil
	// error means "not configured for this cluster" (the source stays dormant
	// unless the cluster opts in, e.g. via annotations).
	Passwords(ctx context.Context, c client.Client, cluster *v2alpha1.Cluster) (map[string]string, error)
}

// passwordSources holds the optional sources compiled into this build. The
// default build leaves it empty; an optional module (e.g. //go:build vault)
// appends to it from its init(). resolvePasswords ranges over it, so it is
// always read — no unused-symbol noise either way.
var passwordSources []PasswordSource

package tarantool3

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
)

// TestCredentialResolutionIsNamespaceConfined is the regression test for RP2
// (REVIEW.md): credential Secrets must be read only from the owning Cluster's
// namespace, never another. A same-named Secret in a foreign namespace must not
// be reachable.
func TestCredentialResolutionIsNamespaceConfined(t *testing.T) {
	// "creds" exists only in the foreign namespace, not in the Cluster's ("ns1").
	foreignSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "victim-ns"},
		Data:       map[string][]byte{"admin": []byte("stolen")},
	}
	c := fake.NewClientBuilder().
		WithScheme(mappingScheme(t)).
		WithObjects(foreignSecret).
		Build()

	cluster := &v2alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "ns1"},
		Spec:       v2alpha1.ClusterSpec{CredentialsSecret: "creds"},
	}
	ctx := context.Background()

	// resolveUserPassword must NOT find the foreign-namespace Secret.
	if _, err := resolveUserPassword(ctx, c, cluster, "admin"); err == nil {
		t.Errorf("resolveUserPassword reached a Secret outside the Cluster's namespace (RP2 not closed)")
	}

	// resolvePasswords must likewise fail rather than read cross-namespace.
	r := &ClusterReconciler{Client: c}
	if _, err := r.resolvePasswords(ctx, cluster); err == nil {
		t.Errorf("resolvePasswords reached a Secret outside the Cluster's namespace (RP2 not closed)")
	}

	// Sanity: the same Secret in the Cluster's own namespace IS resolved.
	ownSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "ns1"},
		Data:       map[string][]byte{"admin": []byte("ok")},
	}
	c2 := fake.NewClientBuilder().WithScheme(mappingScheme(t)).WithObjects(ownSecret).Build()
	got, err := resolveUserPassword(ctx, c2, cluster, "admin")
	if err != nil || got != "ok" {
		t.Errorf("resolveUserPassword(own namespace) = %q, %v; want ok, nil", got, err)
	}
}

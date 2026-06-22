//go:build vault

package tarantool3

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
)

func TestVaultSource(t *testing.T) {
	ctx := context.Background()

	// A mock Vault serving KV v2 at /v1/secret/data/tarantool/c for a known token.
	var gotToken, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-Vault-Token")
		gotPath = r.URL.Path
		if gotToken != "s.testtoken" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"data":{"admin":"a-pw","replicator":"r-pw"}}}`))
	}))
	defer srv.Close()

	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vault-token", Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("s.testtoken")},
	}
	c := fake.NewClientBuilder().WithObjects(tokenSecret).Build()

	clusterWith := func(ann map[string]string) *v2alpha1.Cluster {
		return &v2alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "ns", Annotations: ann}}
	}

	t.Run("dormant when not configured", func(t *testing.T) {
		pw, err := vaultSource{}.Passwords(ctx, c, clusterWith(nil))
		if err != nil || pw != nil {
			t.Errorf("unconfigured Vault should be a no-op, got %v %v", pw, err)
		}
	})

	t.Run("reads KV v2 data map", func(t *testing.T) {
		pw, err := vaultSource{}.Passwords(ctx, c, clusterWith(map[string]string{
			annVaultAddress:     srv.URL,
			annVaultPath:        "secret/data/tarantool/c",
			annVaultTokenSecret: "vault-token",
		}))
		if err != nil {
			t.Fatalf("Passwords: %v", err)
		}
		if pw["admin"] != "a-pw" || pw["replicator"] != "r-pw" {
			t.Errorf("passwords = %v, want admin=a-pw replicator=r-pw", pw)
		}
		if gotPath != "/v1/secret/data/tarantool/c" {
			t.Errorf("vault path = %q", gotPath)
		}
		if gotToken != "s.testtoken" {
			t.Errorf("vault token = %q", gotToken)
		}
	})

	t.Run("missing token secret errors", func(t *testing.T) {
		_, err := vaultSource{}.Passwords(ctx, c, clusterWith(map[string]string{
			annVaultAddress:     srv.URL,
			annVaultPath:        "secret/data/tarantool/c",
			annVaultTokenSecret: "absent",
		}))
		if err == nil {
			t.Errorf("want an error when the token secret is missing")
		}
	})

	t.Run("config without token-secret annotation errors", func(t *testing.T) {
		_, err := vaultSource{}.Passwords(ctx, c, clusterWith(map[string]string{
			annVaultAddress: srv.URL,
			annVaultPath:    "secret/data/tarantool/c",
		}))
		if err == nil {
			t.Errorf("want an error when address+path are set but token-secret is not")
		}
	})

	t.Run("HTTPS with a private CA via vault-ca-secret", func(t *testing.T) {
		tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"data":{"data":{"admin":"tls-pw"}}}`))
		}))
		defer tlsSrv.Close()

		// The httptest CA is private — without the CA secret verification fails.
		ann := map[string]string{
			annVaultAddress:     tlsSrv.URL,
			annVaultPath:        "secret/data/tarantool/c",
			annVaultTokenSecret: "vault-token",
		}
		if _, err := (vaultSource{}).Passwords(ctx, c, clusterWith(ann)); err == nil {
			t.Fatalf("HTTPS with an unknown CA must fail without a CA secret")
		}

		// With the server's CA delivered via the annotation it succeeds.
		caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsSrv.Certificate().Raw})
		caSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "vault-ca", Namespace: "ns"},
			Data:       map[string][]byte{"ca.crt": caPEM},
		}
		cWithCA := fake.NewClientBuilder().WithObjects(tokenSecret, caSecret).Build()
		ann[annVaultCASecret] = "vault-ca"
		pw, err := vaultSource{}.Passwords(ctx, cWithCA, clusterWith(ann))
		if err != nil {
			t.Fatalf("Passwords over TLS with CA secret: %v", err)
		}
		if pw["admin"] != "tls-pw" {
			t.Errorf("passwords = %v, want admin=tls-pw", pw)
		}

		// A CA secret without the ca.crt key is a configuration error.
		badCA := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "vault-ca", Namespace: "ns"}}
		cBadCA := fake.NewClientBuilder().WithObjects(tokenSecret, badCA).Build()
		if _, err := (vaultSource{}).Passwords(ctx, cBadCA, clusterWith(ann)); err == nil {
			t.Errorf("want an error when the CA secret lacks ca.crt")
		}
	})

	t.Run("registered in the vault build", func(t *testing.T) {
		found := false
		for _, s := range passwordSources {
			if s.Name() == "vault" {
				found = true
			}
		}
		if !found {
			t.Errorf("vault source should be registered under the vault build tag")
		}
	})
}

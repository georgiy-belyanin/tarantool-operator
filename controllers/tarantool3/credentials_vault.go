//go:build vault

package tarantool3

// This file is compiled only with `-tags vault`. Without the tag the operator
// has no Vault code and no Vault-related behavior; with it, a cluster opts in
// per-cluster via annotations (no CRD field). Build with: go build -tags vault.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
)

// Cluster annotations that opt a cluster into Vault-sourced passwords. All three
// are required to activate the source; otherwise it stays dormant.
const (
	// annVaultAddress is the Vault base URL, e.g. https://vault.svc:8200
	annVaultAddress = "tarantool.io/vault-address"
	// annVaultPath is the KV v2 data path after /v1/, e.g.
	// secret/data/tarantool/mycluster — its data map is user -> password.
	annVaultPath = "tarantool.io/vault-path"
	// annVaultTokenSecret names an in-namespace Secret holding the Vault token
	// under key "token".
	//nolint:gosec // G101: this is an annotation key name, not a credential
	annVaultTokenSecret = "tarantool.io/vault-token-secret"
	// annVaultCASecret (optional) names an in-namespace Secret holding the CA
	// bundle (key "ca.crt") that signed the Vault server certificate. Most real
	// Vault deployments use HTTPS with a private CA, which the system trust
	// store cannot verify.
	//nolint:gosec // G101: this is an annotation key name, not a credential
	annVaultCASecret = "tarantool.io/vault-ca-secret"
)

func init() { passwordSources = append(passwordSources, vaultSource{}) }

// vaultSource reads Tarantool user passwords from a Vault KV v2 secret. It is a
// deliberately dependency-free implementation (net/http + encoding/json, no
// hashicorp SDK) — a small KV v2 read is all the operator needs.
type vaultSource struct{}

func (vaultSource) Name() string { return "vault" }

func (vaultSource) Passwords(ctx context.Context, c client.Client, cluster *v2alpha1.Cluster) (map[string]string, error) {
	ann := cluster.Annotations
	addr := strings.TrimRight(ann[annVaultAddress], "/")
	path := strings.TrimLeft(ann[annVaultPath], "/")
	tokenSecret := ann[annVaultTokenSecret]
	if addr == "" || path == "" {
		return nil, nil // not configured for this cluster — dormant
	}
	if tokenSecret == "" {
		return nil, fmt.Errorf("annotation %s is required when Vault is configured", annVaultTokenSecret)
	}

	token, err := vaultToken(ctx, c, cluster.Namespace, tokenSecret)
	if err != nil {
		return nil, err
	}
	httpClient, err := vaultHTTPClient(ctx, c, cluster.Namespace, ann[annVaultCASecret])
	if err != nil {
		return nil, err
	}
	return vaultKVRead(ctx, httpClient, addr, path, token)
}

// vaultHTTPClient builds the HTTP client for Vault requests. With no CA secret
// configured it trusts the system roots (public-CA or plain-HTTP Vault); with
// one, only that CA bundle verifies the server — http.DefaultClient is never
// used, so a custom CA cannot leak into unrelated requests and vice versa.
func vaultHTTPClient(ctx context.Context, c client.Client, namespace, caSecret string) (*http.Client, error) {
	httpClient := &http.Client{Timeout: 10 * time.Second}
	if caSecret == "" {
		return httpClient, nil
	}

	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: caSecret}, secret); err != nil {
		return nil, fmt.Errorf("vault CA secret %s/%s: %w", namespace, caSecret, err)
	}
	pem := secret.Data["ca.crt"]
	if len(pem) == 0 {
		return nil, fmt.Errorf("vault CA secret %s/%s has no \"ca.crt\" key", namespace, caSecret)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("vault CA secret %s/%s: \"ca.crt\" contains no valid PEM certificates", namespace, caSecret)
	}
	httpClient.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return httpClient, nil
}

// vaultToken reads the Vault token from the named in-namespace Secret (key "token").
func vaultToken(ctx context.Context, c client.Client, namespace, secretName string) (string, error) {
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, secret); err != nil {
		return "", fmt.Errorf("vault token secret %s/%s: %w", namespace, secretName, err)
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		return "", fmt.Errorf("vault token secret %s/%s has no \"token\" key", namespace, secretName)
	}
	return token, nil
}

// vaultKVRead performs a Vault KV v2 read of addr/v1/<path> and returns the
// secret's data map (user -> password). Response shape: {"data":{"data":{...}}}.
func vaultKVRead(ctx context.Context, httpClient *http.Client, addr, path, token string) (map[string]string, error) {
	url := addr + "/v1/" + path
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Vault-Token", token)

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("vault GET %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Data struct {
			Data map[string]string `json:"data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("vault GET %s: parsing response: %w", url, err)
	}
	return parsed.Data.Data, nil
}

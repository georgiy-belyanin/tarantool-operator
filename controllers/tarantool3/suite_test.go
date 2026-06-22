package tarantool3_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/controllers/tarantool3"
)

// These are integration tests against a real apiserver+etcd via envtest. There
// is no kubelet, so StatefulSets never get running pods; tests assert on the
// objects the reconcilers create and on the not-yet-ready statuses.
//
// Requires the envtest control-plane binaries. Run via `make test`, or set
// KUBEBUILDER_ASSETS (e.g. `KUBEBUILDER_ASSETS=$(bin/setup-envtest use 1.31.0
// --bin-dir bin -p path) go test ./controllers/tarantool3/...`). When the assets
// are absent the suite is skipped rather than failed.

var (
	testEnv   *envtest.Environment
	cfg       *rest.Config
	k8sClient client.Client
	testCtx   context.Context
	cancel    context.CancelFunc
)

const (
	timeout  = 20 * time.Second
	interval = 200 * time.Millisecond
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		// No control-plane binaries available; skip the whole suite.
		os.Exit(runSkipped(m))
	}

	logf.SetLogger(zap.New(zap.UseDevMode(true)))
	testCtx, cancel = context.WithCancel(context.Background())

	testEnv = &envtest.Environment{
		// CRDs live in per-API subdirectories (cartridge/, tarantool3/); these
		// tests only need the Tarantool 3 (db.tarantool.io) CRDs.
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases", "tarantool3")},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		panic("starting envtest: " + err.Error())
	}

	if err := v2alpha1.AddToScheme(scheme.Scheme); err != nil {
		panic(err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:             scheme.Scheme,
		MetricsBindAddress: "0",
	})
	if err != nil {
		panic("creating manager: " + err.Error())
	}
	if err := tarantool3.NewClusterReconciler(mgr).SetupWithManager(mgr); err != nil {
		panic(err)
	}
	if err := tarantool3.NewReplicaSetReconciler(mgr).SetupWithManager(mgr); err != nil {
		panic(err)
	}

	// A direct (uncached) client for test setup and assertions, so reads are not
	// subject to informer cache lag.
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	if err != nil {
		panic(err)
	}

	go func() {
		if err := mgr.Start(testCtx); err != nil {
			panic("starting manager: " + err.Error())
		}
	}()

	code := m.Run()

	cancel()
	_ = testEnv.Stop()
	os.Exit(code)
}

// runSkipped runs the suite with a flag that makes every test skip. It prints a
// loud banner first: a bare `go test` without KUBEBUILDER_ASSETS otherwise reports
// a green `ok` while every integration test was a no-op (see TEST-PROBLEMS.md P1).
func runSkipped(m *testing.M) int {
	fmt.Fprintln(os.Stderr, "WARNING: KUBEBUILDER_ASSETS unset — SKIPPING all envtest integration tests "+
		"(this run does NOT validate the controllers). Use `make test`, which provisions the control-plane binaries.")
	skipAll = true
	return m.Run()
}

var skipAll bool

func skipIfNoEnvtest(t *testing.T) {
	t.Helper()
	if skipAll {
		t.Skip("envtest control-plane binaries not available (set KUBEBUILDER_ASSETS)")
	}
}

// credentialSecret builds a Secret holding user passwords.
func credentialSecret(name string, data map[string]string) *corev1.Secret {
	d := map[string][]byte{}
	for k, v := range data {
		d[k] = []byte(v)
	}
	return &corev1.Secret{
		ObjectMeta: objectMeta(name),
		Data:       d,
	}
}

package tarantool3_test

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/controllers/tarantool3"
)

// TestConfigSecretStableAcrossReconciles is the regression test for RP1 (REVIEW.md):
// the rendered config is not byte-stable, and the Cluster controller Owns/Watches
// the config Secret, so writing Data unconditionally re-enqueues the Cluster and
// rewrites the Secret forever. Once the config has converged, the Secret's
// ResourceVersion must stay put — a steady-state reconcile is a no-op.
func TestConfigSecretStableAcrossReconciles(t *testing.T) {
	skipIfNoEnvtest(t)
	g := NewWithT(t)
	ctx := context.Background()
	name := "cl-idem"

	cluster := &v2alpha1.Cluster{
		ObjectMeta: objectMeta(name),
		Spec: v2alpha1.ClusterSpec{
			Config: jsonExt(`{"replication":{"failover":"election"}}`),
		},
	}
	g.Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	g.Expect(k8sClient.Create(ctx, newReplicaSet(name+"-storage-001", name, "storages", 2, "storage"))).To(Succeed())

	secretName := tarantool3.ConfigSecretName(name)
	cfg := &corev1.Secret{}
	g.Eventually(func() error { return k8sClient.Get(ctx, nn(secretName), cfg) }, timeout, interval).Should(Succeed())

	// Wait until the ResourceVersion is stable across two successive reads — i.e.
	// reconciliation has converged (the initial create + the ReplicaSet-triggered
	// re-render have settled).
	prev := ""
	g.Eventually(func() bool {
		if err := k8sClient.Get(ctx, nn(secretName), cfg); err != nil {
			return false
		}
		stable := cfg.ResourceVersion == prev
		prev = cfg.ResourceVersion
		return stable
	}, timeout, interval).Should(BeTrue())

	rv := cfg.ResourceVersion

	// With the bug, the controller's own write re-enqueues it and the Secret's
	// ResourceVersion climbs continuously. With the fix it must not move.
	g.Consistently(func() string {
		_ = k8sClient.Get(ctx, nn(secretName), cfg)
		return cfg.ResourceVersion
	}, 5*interval, interval).Should(Equal(rv), "config Secret must not be rewritten on no-op reconciles (RP1)")
}

// TestConfigSecretRewrittenOnRealChange is the counterpart: when the config
// genuinely changes, the Secret must be updated (the hash gate must not wedge it).
func TestConfigSecretRewrittenOnRealChange(t *testing.T) {
	skipIfNoEnvtest(t)
	g := NewWithT(t)
	ctx := context.Background()
	name := "cl-idem-change"

	cluster := &v2alpha1.Cluster{
		ObjectMeta: objectMeta(name),
		Spec: v2alpha1.ClusterSpec{
			Config: jsonExt(`{"replication":{"failover":"election"}}`),
		},
	}
	g.Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

	secretName := tarantool3.ConfigSecretName(name)
	cfg := &corev1.Secret{}
	g.Eventually(func() error { return k8sClient.Get(ctx, nn(secretName), cfg) }, timeout, interval).Should(Succeed())
	firstHash := cfg.Annotations[tarantool3.ConfigHashAnnotation]
	g.Expect(firstHash).NotTo(BeEmpty())

	// Change the cluster config (a mutable global key); the Secret's hash annotation
	// must change. (bucketCount is immutable, so use replication.timeout.)
	g.Eventually(func() error {
		if err := k8sClient.Get(ctx, nn(name), cluster); err != nil {
			return err
		}
		cluster.Spec.Config = jsonExt(`{"replication":{"failover":"election","timeout":5}}`)
		return k8sClient.Update(ctx, cluster)
	}, timeout, interval).Should(Succeed())

	g.Eventually(func() string {
		_ = k8sClient.Get(ctx, nn(secretName), cfg)
		return cfg.Annotations[tarantool3.ConfigHashAnnotation]
	}, timeout, interval).ShouldNot(Equal(firstHash), "a real config change must rewrite the Secret")
}

// TestUnrelatedReplicaSetDoesNotRollThisOne is the P3 regression test: adding a
// second replica set must not change the first replica set's StatefulSet pod
// template (its scoped config hash), so unrelated replica sets are not rolled.
func TestUnrelatedReplicaSetDoesNotRollThisOne(t *testing.T) {
	skipIfNoEnvtest(t)
	g := NewWithT(t)
	ctx := context.Background()
	name := "cl-p3"

	cluster := &v2alpha1.Cluster{
		ObjectMeta: objectMeta(name),
		Spec:       v2alpha1.ClusterSpec{Config: jsonExt(`{"replication":{"failover":"election"}}`)},
	}
	g.Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	g.Expect(k8sClient.Create(ctx, newReplicaSet(name+"-a", name, "group-a", 1))).To(Succeed())

	// Capture replica set A's StatefulSet pod-template config hash.
	stsA := &appsv1.StatefulSet{}
	g.Eventually(func() error { return k8sClient.Get(ctx, nn(name+"-a"), stsA) }, timeout, interval).Should(Succeed())
	hashA := stsA.Spec.Template.Annotations[tarantool3.ConfigHashAnnotation]
	g.Expect(hashA).NotTo(BeEmpty())

	// Add an unrelated replica set B and wait until the operator has rendered it
	// into the shared config Secret (so A has genuinely been re-reconciled).
	g.Expect(k8sClient.Create(ctx, newReplicaSet(name+"-b", name, "group-b", 1))).To(Succeed())
	stsB := &appsv1.StatefulSet{}
	g.Eventually(func() error { return k8sClient.Get(ctx, nn(name+"-b"), stsB) }, timeout, interval).Should(Succeed())

	// A's pod template (its scoped hash) must not change just because B was added.
	g.Consistently(func() string {
		_ = k8sClient.Get(ctx, nn(name+"-a"), stsA)
		return stsA.Spec.Template.Annotations[tarantool3.ConfigHashAnnotation]
	}, 5*interval, interval).Should(Equal(hashA), "adding an unrelated replica set must not roll this one (P3)")
}

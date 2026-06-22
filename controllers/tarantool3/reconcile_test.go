package tarantool3_test

import (
	"context"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/controllers/tarantool3"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

const testNamespace = "default"

func objectMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: testNamespace}
}

func ptr[T any](v T) *T { return &v }

func jsonExt(s string) *apiextensionsv1.JSON { return &apiextensionsv1.JSON{Raw: []byte(s)} }

func nn(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: testNamespace, Name: name}
}

func newReplicaSet(name, clusterName, group string, replicas int32, shardingRoles ...string) *v2alpha1.ReplicaSet {
	var cfg *apiextensionsv1.JSON
	if len(shardingRoles) > 0 {
		cfg = jsonExt(`{"sharding":{"roles":["` + strings.Join(shardingRoles, `","`) + `"]}}`)
	}
	return &v2alpha1.ReplicaSet{
		ObjectMeta: objectMeta(name),
		Spec: v2alpha1.ReplicaSetSpec{
			ClusterName: clusterName,
			Group:       group,
			Replicas:    ptr(replicas),
			Config:      cfg,
			PodTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "tarantool", Image: "tarantool/tarantool:3"}},
				},
			},
		},
	}
}

func ownedBy(refs []metav1.OwnerReference, kind, name string) bool {
	for _, r := range refs {
		if r.Kind == kind && r.Name == name {
			return true
		}
	}
	return false
}

// TestOperatorUserDefault: with no credentials configured at all, the operator
// provisions its built-in super user — generated password Secret plus the user
// in the rendered config — and removes both when explicitly disabled.
func TestOperatorUserDefault(t *testing.T) {
	skipIfNoEnvtest(t)
	g := NewWithT(t)
	ctx := context.Background()
	name := "opuser"

	cluster := &v2alpha1.Cluster{ObjectMeta: objectMeta(name)}
	g.Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	g.Expect(k8sClient.Create(ctx, newReplicaSet(name+"-rs", name, "routers", 1))).To(Succeed())

	// Generated password Secret appears, keyed by the user name, owned by the Cluster.
	pwSecret := &corev1.Secret{}
	g.Eventually(func() error {
		return k8sClient.Get(ctx, nn(tarantool3.OperatorUserSecretName(name)), pwSecret)
	}, timeout, interval).Should(Succeed())
	g.Expect(pwSecret.Data).To(HaveKey(clusterconfig.OperatorUser))
	g.Expect(pwSecret.Data[clusterconfig.OperatorUser]).NotTo(BeEmpty())
	g.Expect(ownedBy(pwSecret.OwnerReferences, "Cluster", name)).To(BeTrue())

	// The rendered config declares the user with the super role and its password.
	cfgSecret := &corev1.Secret{}
	g.Eventually(func() bool {
		if err := k8sClient.Get(ctx, nn(tarantool3.ConfigSecretName(name)), cfgSecret); err != nil {
			return false
		}
		var tree map[string]any
		if yaml.Unmarshal(cfgSecret.Data[tarantool3.ConfigFileName], &tree) != nil {
			return false
		}
		users, _ := tree["credentials"].(map[string]any)["users"].(map[string]any)
		u, _ := users[clusterconfig.OperatorUser].(map[string]any)
		pw, _ := u["password"].(string)
		return pw == string(pwSecret.Data[clusterconfig.OperatorUser])
	}, timeout, interval).Should(BeTrue(), "config must carry the operator user with the generated password")

	// Disabling removes the Secret and the user from the rendered config.
	// (Retried: the controller's status updates race this spec update.)
	g.Eventually(func() error {
		if err := k8sClient.Get(ctx, nn(name), cluster); err != nil {
			return err
		}
		cluster.Spec.DisableOperatorUser = true
		return k8sClient.Update(ctx, cluster)
	}, timeout, interval).Should(Succeed())

	g.Eventually(func() bool {
		err := k8sClient.Get(ctx, nn(tarantool3.OperatorUserSecretName(name)), &corev1.Secret{})
		return apierrors.IsNotFound(err)
	}, timeout, interval).Should(BeTrue(), "operator-user Secret must be removed when disabled")
	g.Eventually(func() bool {
		if err := k8sClient.Get(ctx, nn(tarantool3.ConfigSecretName(name)), cfgSecret); err != nil {
			return false
		}
		var tree map[string]any
		if yaml.Unmarshal(cfgSecret.Data[tarantool3.ConfigFileName], &tree) != nil {
			return false
		}
		creds, _ := tree["credentials"].(map[string]any)
		users, _ := creds["users"].(map[string]any)
		_, has := users[clusterconfig.OperatorUser]
		return !has
	}, timeout, interval).Should(BeTrue(), "config must drop the operator user when disabled")
}

// TestSelectorFieldsImmutable: clusterName and group are baked into the
// StatefulSet's immutable pod selector, so the CRD must reject changing them
// (CEL XValidation) instead of letting every later reconcile fail on a
// selector update.
func TestSelectorFieldsImmutable(t *testing.T) {
	skipIfNoEnvtest(t)
	g := NewWithT(t)
	ctx := context.Background()

	rs := newReplicaSet("immutable-rs", "immutable-cl", "groupa", 1)
	g.Expect(k8sClient.Create(ctx, rs)).To(Succeed())

	rs.Spec.Group = "groupb"
	err := k8sClient.Update(ctx, rs)
	g.Expect(err).To(HaveOccurred(), "changing spec.group must be rejected")
	g.Expect(err.Error()).To(ContainSubstring("immutable"))

	g.Expect(k8sClient.Get(ctx, nn("immutable-rs"), rs)).To(Succeed())
	rs.Spec.ClusterName = "another-cluster"
	err = k8sClient.Update(ctx, rs)
	g.Expect(err).To(HaveOccurred(), "changing spec.clusterName must be rejected")
	g.Expect(err.Error()).To(ContainSubstring("immutable"))
}

func TestClusterReconcileCreatesServiceAndConfigSecret(t *testing.T) {
	skipIfNoEnvtest(t)
	g := NewWithT(t)
	ctx := context.Background()
	name := "cl-render"

	g.Expect(k8sClient.Create(ctx, credentialSecret(name+"-creds", map[string]string{
		"replicator": "r-secret",
		"storage":    "s-secret",
	}))).To(Succeed())

	cluster := &v2alpha1.Cluster{
		ObjectMeta: objectMeta(name),
		Spec: v2alpha1.ClusterSpec{
			Config:            jsonExt(`{"replication":{"failover":"election"},"credentials":{"users":{"replicator":{"roles":["replication"]},"storage":{"roles":["sharding"]}}}}`),
			CredentialsSecret: name + "-creds",
		},
	}
	g.Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	g.Expect(k8sClient.Create(ctx, newReplicaSet(name+"-router", name, "routers", 1, "router"))).To(Succeed())

	// Headless service.
	svc := &corev1.Service{}
	g.Eventually(func() error { return k8sClient.Get(ctx, nn(name), svc) }, timeout, interval).Should(Succeed())
	g.Expect(svc.Spec.ClusterIP).To(Equal(corev1.ClusterIPNone))
	g.Expect(ownedBy(svc.OwnerReferences, "Cluster", name)).To(BeTrue())

	// Rendered config Secret.
	cfgSecret := &corev1.Secret{}
	g.Eventually(func() error {
		return k8sClient.Get(ctx, nn(tarantool3.ConfigSecretName(name)), cfgSecret)
	}, timeout, interval).Should(Succeed())
	g.Expect(cfgSecret.Data).To(HaveKey(tarantool3.ConfigFileName))
	g.Expect(cfgSecret.Annotations).To(HaveKey(tarantool3.ConfigHashAnnotation))
	g.Expect(ownedBy(cfgSecret.OwnerReferences, "Cluster", name)).To(BeTrue())

	// The rendered config is a valid T3 tree with credentials and the router group.
	var tree map[string]any
	g.Expect(yaml.Unmarshal(cfgSecret.Data[tarantool3.ConfigFileName], &tree)).To(Succeed())
	g.Eventually(func() bool {
		_ = k8sClient.Get(ctx, nn(tarantool3.ConfigSecretName(name)), cfgSecret)
		_ = yaml.Unmarshal(cfgSecret.Data[tarantool3.ConfigFileName], &tree)
		groups, _ := tree["groups"].(map[string]any)
		_, hasRouters := groups["routers"]
		return hasRouters
	}, timeout, interval).Should(BeTrue(), "config should include the routers group once the ReplicaSet is observed")
	g.Expect(tree).To(HaveKey("credentials"))

	// Status reaches Ready, with a Ready=True condition.
	g.Eventually(func() v2alpha1.ClusterPhase {
		_ = k8sClient.Get(ctx, nn(name), cluster)
		return cluster.Status.Phase
	}, timeout, interval).Should(Equal(v2alpha1.ClusterReady))
	g.Expect(meta.IsStatusConditionTrue(cluster.Status.Conditions, tarantool3.ConditionReady)).To(BeTrue())
}

func TestReplicaSetReconcileCreatesStatefulSet(t *testing.T) {
	skipIfNoEnvtest(t)
	g := NewWithT(t)
	ctx := context.Background()
	name := "cl-sts"

	cluster := &v2alpha1.Cluster{
		ObjectMeta: objectMeta(name),
		Spec:       v2alpha1.ClusterSpec{Config: jsonExt(`{"replication":{"failover":"election"}}`)},
	}
	g.Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

	rsName := name + "-storage-001"
	g.Expect(k8sClient.Create(ctx, newReplicaSet(rsName, name, "storages", 2, "storage"))).To(Succeed())

	sts := &appsv1.StatefulSet{}
	g.Eventually(func() error { return k8sClient.Get(ctx, nn(rsName), sts) }, timeout, interval).Should(Succeed())

	g.Expect(sts.Spec.ServiceName).To(Equal(name))
	g.Expect(sts.Spec.Replicas).To(Equal(ptr(int32(2))))
	g.Expect(ownedBy(sts.OwnerReferences, "ReplicaSet", rsName)).To(BeTrue())

	// Config Secret mounted and the container pointed at it.
	var mounted bool
	for _, v := range sts.Spec.Template.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == tarantool3.ConfigSecretName(name) {
			mounted = true
		}
	}
	g.Expect(mounted).To(BeTrue(), "rendered config Secret must be mounted")
	g.Expect(sts.Spec.Template.Annotations).To(HaveKey(tarantool3.ConfigHashAnnotation))

	var envNames []string
	for _, e := range sts.Spec.Template.Spec.Containers[0].Env {
		envNames = append(envNames, e.Name)
	}
	g.Expect(envNames).To(ContainElements("TT_CONFIG", "TT_INSTANCE_NAME"))

	// No kubelet under envtest, so it stays Configuring with 0 ready instances.
	got := &v2alpha1.ReplicaSet{}
	g.Eventually(func() string {
		_ = k8sClient.Get(ctx, nn(rsName), got)
		return string(got.Status.Phase) + " " + got.Status.ReadyInstances
	}, timeout, interval).Should(Equal(string(v2alpha1.ReplicaSetConfiguring) + " 0/2"))

	// The scale subresource (F13) needs status.selector populated with a selector
	// that matches this replica set's instance pods.
	g.Expect(got.Status.Selector).To(ContainSubstring(tarantool3.ReplicaSetLabel + "=" + rsName))
	sel, err := labels.Parse(got.Status.Selector)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(sel.Matches(labels.Set{
		tarantool3.ReplicaSetLabel: rsName,
		tarantool3.ClusterLabel:    name,
		tarantool3.GroupLabel:      "storages",
		tarantool3.ManagedByLabel:  tarantool3.ManagedByValue,
	})).To(BeTrue())
}

func TestMissingCredentialSecretRecovers(t *testing.T) {
	skipIfNoEnvtest(t)
	g := NewWithT(t)
	ctx := context.Background()
	name := "cl-missing"
	secretName := name + "-creds"

	cluster := &v2alpha1.Cluster{
		ObjectMeta: objectMeta(name),
		Spec: v2alpha1.ClusterSpec{
			Config:            jsonExt(`{"credentials":{"users":{"replicator":{"roles":["replication"]}}}}`),
			CredentialsSecret: secretName,
		},
	}
	g.Expect(k8sClient.Create(ctx, cluster)).To(Succeed())

	// Secret missing → Pending (transient), and no config Secret produced yet.
	g.Eventually(func() v2alpha1.ClusterPhase {
		_ = k8sClient.Get(ctx, nn(name), cluster)
		return cluster.Status.Phase
	}, timeout, interval).Should(Equal(v2alpha1.ClusterPending))
	g.Consistently(func() bool {
		return k8sClient.Get(ctx, nn(tarantool3.ConfigSecretName(name)), &corev1.Secret{}) != nil
	}, 2*interval, interval).Should(BeTrue())

	// Create the credential Secret → the cluster must recover to Ready and the
	// config Secret must appear (the Secret watch re-triggers reconciliation).
	g.Expect(k8sClient.Create(ctx, credentialSecret(secretName, map[string]string{"replicator": "r-secret"}))).To(Succeed())

	g.Eventually(func() v2alpha1.ClusterPhase {
		_ = k8sClient.Get(ctx, nn(name), cluster)
		return cluster.Status.Phase
	}, timeout, interval).Should(Equal(v2alpha1.ClusterReady))
	g.Eventually(func() error {
		return k8sClient.Get(ctx, nn(tarantool3.ConfigSecretName(name)), &corev1.Secret{})
	}, timeout, interval).Should(Succeed())
}

func TestReplicaSetCreatesPodDisruptionBudget(t *testing.T) {
	skipIfNoEnvtest(t)
	g := NewWithT(t)
	ctx := context.Background()
	name := "cl-pdb"

	cluster := &v2alpha1.Cluster{
		ObjectMeta: objectMeta(name),
		Spec:       v2alpha1.ClusterSpec{Config: jsonExt(`{"replication":{"failover":"election"}}`)},
	}
	g.Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	rsName := name + "-storage-001"
	g.Expect(k8sClient.Create(ctx, newReplicaSet(rsName, name, "storages", 3))).To(Succeed())

	// A 3-instance set gets a PDB with maxUnavailable=1, owned by the ReplicaSet.
	pdb := &policyv1.PodDisruptionBudget{}
	g.Eventually(func() error {
		return k8sClient.Get(ctx, nn(tarantool3.PDBName(rsName)), pdb)
	}, timeout, interval).Should(Succeed())
	g.Expect(pdb.Spec.MaxUnavailable).NotTo(BeNil())
	g.Expect(pdb.Spec.MaxUnavailable.IntValue()).To(Equal(1))
	g.Expect(ownedBy(pdb.OwnerReferences, "ReplicaSet", rsName)).To(BeTrue())

	// Scaling to a single instance removes the PDB (nothing to protect; must not
	// block draining the lone node).
	g.Eventually(func() error {
		if err := k8sClient.Get(ctx, nn(rsName), &v2alpha1.ReplicaSet{}); err != nil {
			return err
		}
		got := &v2alpha1.ReplicaSet{}
		_ = k8sClient.Get(ctx, nn(rsName), got)
		got.Spec.Replicas = ptr(int32(1))
		return k8sClient.Update(ctx, got)
	}, timeout, interval).Should(Succeed())

	g.Eventually(func() bool {
		return k8sClient.Get(ctx, nn(tarantool3.PDBName(rsName)), &policyv1.PodDisruptionBudget{}) != nil
	}, timeout, interval).Should(BeTrue(), "PDB should be removed when scaled to a single instance")
}

func TestReplicaSetWithoutClusterIsPending(t *testing.T) {
	skipIfNoEnvtest(t)
	g := NewWithT(t)
	ctx := context.Background()
	rsName := "rs-no-cluster"

	// A ReplicaSet referencing a Cluster that does not exist must not create a
	// StatefulSet; it waits in Pending until the Cluster (and its config Secret)
	// appear.
	g.Expect(k8sClient.Create(ctx, newReplicaSet(rsName, "missing-cluster", "storages", 1, "storage"))).To(Succeed())

	g.Eventually(func() v2alpha1.ReplicaSetPhase {
		got := &v2alpha1.ReplicaSet{}
		_ = k8sClient.Get(ctx, nn(rsName), got)
		return got.Status.Phase
	}, timeout, interval).Should(Equal(v2alpha1.ReplicaSetPending))

	g.Consistently(func() bool {
		// No StatefulSet should be created while the cluster is absent.
		return k8sClient.Get(ctx, nn(rsName), &appsv1.StatefulSet{}) != nil
	}, 2*interval, interval).Should(BeTrue(), "no StatefulSet until the Cluster exists")
}

func TestConfigChangeRollsStatefulSetHash(t *testing.T) {
	skipIfNoEnvtest(t)
	g := NewWithT(t)
	ctx := context.Background()
	name := "cl-roll"

	cluster := &v2alpha1.Cluster{
		ObjectMeta: objectMeta(name),
		Spec:       v2alpha1.ClusterSpec{Config: jsonExt(`{"replication":{"failover":"election"}}`)},
	}
	g.Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	rsName := name + "-storage-001"
	g.Expect(k8sClient.Create(ctx, newReplicaSet(rsName, name, "storages", 1, "storage"))).To(Succeed())

	sts := &appsv1.StatefulSet{}
	g.Eventually(func() error { return k8sClient.Get(ctx, nn(rsName), sts) }, timeout, interval).Should(Succeed())
	firstHash := sts.Spec.Template.Annotations[tarantool3.ConfigHashAnnotation]
	g.Expect(firstHash).NotTo(BeEmpty())

	// Change a STATIC global key; the new hash must propagate to the pod
	// template. (A dynamic key — log.level, replication.timeout, ... — would
	// deliberately NOT roll: the built-in operator user makes hot reload
	// available by default, so the controller stamps RolloutHash, which
	// excludes dynamic keys.)
	g.Eventually(func() error {
		if err := k8sClient.Get(ctx, nn(name), cluster); err != nil {
			return err
		}
		cluster.Spec.Config = jsonExt(`{"replication":{"failover":"election"},"iproto":{"threads":2}}`)
		return k8sClient.Update(ctx, cluster)
	}, timeout, interval).Should(Succeed())

	g.Eventually(func() string {
		_ = k8sClient.Get(ctx, nn(rsName), sts)
		return sts.Spec.Template.Annotations[tarantool3.ConfigHashAnnotation]
	}, timeout, interval).ShouldNot(Equal(firstHash), "config change must re-stamp the pod template hash")
}

// TestStuckPodRemediation (F3): an instance pod stranded on an unreachable node
// past the grace period is force-deleted by the reconciler, so the StatefulSet
// can recreate it. envtest has no kubelet, so STS readyReplicas is set manually
// to satisfy the bootstrapped/quorum gates.
func TestStuckPodRemediation(t *testing.T) {
	skipIfNoEnvtest(t)
	g := NewWithT(t)
	ctx := context.Background()
	name := "cl-rem"
	rsName := name + "-rs"

	cluster := &v2alpha1.Cluster{
		ObjectMeta: objectMeta(name),
		Spec:       v2alpha1.ClusterSpec{Config: jsonExt(`{"replication":{"failover":"election"}}`)},
	}
	g.Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
	g.Expect(k8sClient.Create(ctx, newReplicaSet(rsName, name, "storages", 3))).To(Succeed())

	sts := &appsv1.StatefulSet{}
	g.Eventually(func() error { return k8sClient.Get(ctx, nn(rsName), sts) }, timeout, interval).Should(Succeed())

	// Mark the set fully ready (no kubelet in envtest) so it becomes Bootstrapped.
	g.Eventually(func() error {
		if err := k8sClient.Get(ctx, nn(rsName), sts); err != nil {
			return err
		}
		sts.Status.Replicas = 3
		sts.Status.ReadyReplicas = 3
		return k8sClient.Status().Update(ctx, sts)
	}, timeout, interval).Should(Succeed())
	g.Eventually(func() bool {
		got := &v2alpha1.ReplicaSet{}
		_ = k8sClient.Get(ctx, nn(rsName), got)
		return got.Status.Bootstrapped
	}, timeout, interval).Should(BeTrue())

	// A node that went unreachable beyond the grace period, and an instance pod on it.
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "rem-dead-node"}}
	g.Expect(k8sClient.Create(ctx, node)).To(Succeed())
	node.Status.Conditions = []corev1.NodeCondition{{
		Type: corev1.NodeReady, Status: corev1.ConditionUnknown,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
	}}
	g.Expect(k8sClient.Status().Update(ctx, node)).To(Succeed())

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: rsName + "-1", Namespace: testNamespace,
			Labels: map[string]string{tarantool3.ReplicaSetLabel: rsName},
		},
		Spec: corev1.PodSpec{
			NodeName:   "rem-dead-node",
			Containers: []corev1.Container{{Name: "tarantool", Image: "tarantool/tarantool:3"}},
		},
	}
	g.Expect(k8sClient.Create(ctx, pod)).To(Succeed())

	// Nudge a reconcile (pods aren't watched) and expect the stuck pod to be gone.
	g.Eventually(func() error {
		got := &v2alpha1.ReplicaSet{}
		if err := k8sClient.Get(ctx, nn(rsName), got); err != nil {
			return err
		}
		if got.Annotations == nil {
			got.Annotations = map[string]string{}
		}
		got.Annotations["nudge"] = time.Now().Format(time.RFC3339Nano)
		return k8sClient.Update(ctx, got)
	}, timeout, interval).Should(Succeed())

	g.Eventually(func() bool {
		err := k8sClient.Get(ctx, nn(rsName+"-1"), &corev1.Pod{})
		return apierrors.IsNotFound(err)
	}, timeout, interval).Should(BeTrue(), "stuck pod should be force-deleted by remediation")
}

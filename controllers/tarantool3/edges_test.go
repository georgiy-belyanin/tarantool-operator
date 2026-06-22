package tarantool3

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// statusClient builds a fake client with status subresources enabled, so the
// reconcilers' status writes work.
func statusClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(mappingScheme(t)).
		WithStatusSubresource(&v2alpha1.Cluster{}, &v2alpha1.ReplicaSet{}).
		WithObjects(objs...).
		Build()
}

func rawJSON(s string) *apiextensionsv1.JSON { return &apiextensionsv1.JSON{Raw: []byte(s)} }

func reqFor(ns, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
}

// TestReplicaSetReconcileEdges covers the early-exit paths before the
// StatefulSet is ever touched.
func TestReplicaSetReconcileEdges(t *testing.T) {
	ctx := context.Background()
	mkRS := func() *v2alpha1.ReplicaSet {
		replicas := int32(1)
		return &v2alpha1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: "edge-rs", Namespace: "default"},
			Spec:       v2alpha1.ReplicaSetSpec{ClusterName: "edge", Replicas: &replicas},
		}
	}
	mkCluster := func() *v2alpha1.Cluster {
		return &v2alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "default"}}
	}

	t.Run("missing object: no error, no requeue", func(t *testing.T) {
		r := &ReplicaSetReconciler{Client: statusClient(t)}
		if _, err := r.Reconcile(ctx, reqFor("default", "nope")); err != nil {
			t.Errorf("Reconcile(absent) error = %v, want nil", err)
		}
	})

	t.Run("deleting object: no-op", func(t *testing.T) {
		rs := mkRS()
		now := metav1.Now()
		rs.DeletionTimestamp = &now
		rs.Finalizers = []string{"test/keep"} // fake client requires one to store a deleting object
		r := &ReplicaSetReconciler{Client: statusClient(t, rs)}
		if _, err := r.Reconcile(ctx, reqFor("default", "edge-rs")); err != nil {
			t.Errorf("Reconcile(deleting) error = %v, want nil", err)
		}
	})

	t.Run("cluster not created yet: Pending", func(t *testing.T) {
		rs := mkRS()
		r := &ReplicaSetReconciler{Client: statusClient(t, rs)}
		if _, err := r.Reconcile(ctx, reqFor("default", "edge-rs")); err != nil {
			t.Fatalf("Reconcile error = %v", err)
		}
		got := &v2alpha1.ReplicaSet{}
		_ = r.Get(ctx, types.NamespacedName{Namespace: "default", Name: "edge-rs"}, got)
		if got.Status.Phase != v2alpha1.ReplicaSetPending {
			t.Errorf("phase = %q, want Pending while the cluster is absent", got.Status.Phase)
		}
	})

	t.Run("config secret not rendered yet: Pending", func(t *testing.T) {
		rs := mkRS()
		r := &ReplicaSetReconciler{Client: statusClient(t, rs, mkCluster())}
		if _, err := r.Reconcile(ctx, reqFor("default", "edge-rs")); err != nil {
			t.Fatalf("Reconcile error = %v", err)
		}
		got := &v2alpha1.ReplicaSet{}
		_ = r.Get(ctx, types.NamespacedName{Namespace: "default", Name: "edge-rs"}, got)
		if got.Status.Phase != v2alpha1.ReplicaSetPending {
			t.Errorf("phase = %q, want Pending while the config secret is absent", got.Status.Phase)
		}
	})

	t.Run("corrupt config secret: error", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: ConfigSecretName("edge"), Namespace: "default"},
			Data:       map[string][]byte{ConfigFileName: []byte("\tnot yaml")},
		}
		r := &ReplicaSetReconciler{Client: statusClient(t, mkRS(), mkCluster(), secret)}
		if _, err := r.Reconcile(ctx, reqFor("default", "edge-rs")); err == nil {
			t.Errorf("Reconcile with a corrupt config secret should error")
		}
	})
}

// TestClusterReconcileEdges covers the Cluster reconciler's early exits and the
// render-failure path.
func TestClusterReconcileEdges(t *testing.T) {
	ctx := context.Background()

	t.Run("missing object: no error", func(t *testing.T) {
		r := &ClusterReconciler{Client: statusClient(t)}
		if _, err := r.Reconcile(ctx, reqFor("default", "nope")); err != nil {
			t.Errorf("Reconcile(absent) error = %v, want nil", err)
		}
	})

	t.Run("deleting object: no-op", func(t *testing.T) {
		c := &v2alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "default"}}
		now := metav1.Now()
		c.DeletionTimestamp = &now
		c.Finalizers = []string{"test/keep"}
		r := &ClusterReconciler{Client: statusClient(t, c)}
		if _, err := r.Reconcile(ctx, reqFor("default", "edge")); err != nil {
			t.Errorf("Reconcile(deleting) error = %v, want nil", err)
		}
	})

	t.Run("unrenderable spec.config: phase Error", func(t *testing.T) {
		// Valid JSON (the apiserver guarantees that much) but not an object —
		// the render rejects it and the cluster must surface Error, not fail.
		c := &v2alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "edge", Namespace: "default"}}
		c.Spec.Config = rawJSON(`[1,2,3]`)
		r := &ClusterReconciler{Client: statusClient(t, c), Scheme: mappingScheme(t), Recorder: record.NewFakeRecorder(8)}
		if _, err := r.Reconcile(ctx, reqFor("default", "edge")); err != nil {
			t.Fatalf("Reconcile error = %v (render failure must set status, not fail)", err)
		}
		got := &v2alpha1.Cluster{}
		_ = r.Get(ctx, types.NamespacedName{Namespace: "default", Name: "edge"}, got)
		if got.Status.Phase != v2alpha1.ClusterError {
			t.Errorf("phase = %q, want Error on a render failure", got.Status.Phase)
		}
	})
}

// TestReplicaSetsForClusterFilters: only ReplicaSets naming this cluster are
// rendered into its config.
func TestReplicaSetsForClusterFilters(t *testing.T) {
	mk := func(name, clusterName string) *v2alpha1.ReplicaSet {
		return &v2alpha1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       v2alpha1.ReplicaSetSpec{ClusterName: clusterName},
		}
	}
	cluster := &v2alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "mine", Namespace: "default"}}
	r := &ClusterReconciler{Client: statusClient(t, mk("a", "mine"), mk("b", "other"))}

	got, err := r.replicaSetsForCluster(context.Background(), cluster)
	if err != nil {
		t.Fatalf("replicaSetsForCluster error = %v", err)
	}
	if len(got) != 1 || got[0].Name != "a" {
		t.Errorf("got %d replicasets, want exactly [a]: %v", len(got), got)
	}
}

// TestObserveLeaderDeclaredModes: in manual/off the operator declares the
// writable instance without any network call; in election the first reachable
// instance's answer is the leader.
func TestObserveLeaderDeclaredModes(t *testing.T) {
	ctx := context.Background()

	t.Run("manual without a configured leader defaults to the first instance", func(t *testing.T) {
		r := &ReplicaSetReconciler{}
		rs := &v2alpha1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs"}}
		got := r.observeLeader(ctx, &v2alpha1.Cluster{}, rs, clusterconfig.Settings{Failover: "manual"})
		if got != "rs-0" {
			t.Errorf("observeLeader(manual, no leader) = %q, want rs-0", got)
		}
	})

	t.Run("off mode: first instance is the writable one", func(t *testing.T) {
		r := &ReplicaSetReconciler{}
		rs := &v2alpha1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs"}}
		got := r.observeLeader(ctx, &v2alpha1.Cluster{}, rs, clusterconfig.Settings{Failover: "off"})
		if got != "rs-0" {
			t.Errorf("observeLeader(off) = %q, want rs-0", got)
		}
	})

	t.Run("off mode: a user-designated rw instance wins over ordinal 0", func(t *testing.T) {
		r := &ReplicaSetReconciler{}
		rs := &v2alpha1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs"}}
		got := r.observeLeader(ctx, &v2alpha1.Cluster{}, rs, clusterconfig.Settings{Failover: "off", OffModeRW: "rs-2"})
		if got != "rs-2" {
			t.Errorf("observeLeader(off, rw on rs-2) = %q, want rs-2", got)
		}
	})

	t.Run("election: the reachable instance's answer wins", func(t *testing.T) {
		orig := evalInstanceString
		t.Cleanup(func() { evalInstanceString = orig })
		evalInstanceString = func(context.Context, string, string, string, string, time.Duration) (string, error) {
			return "rs-2", nil
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
			Data:       map[string][]byte{"admin": []byte("pw")},
		}
		cluster := &v2alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"},
			Spec:       v2alpha1.ClusterSpec{CredentialsSecret: "creds"},
		}
		rs := &v2alpha1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "default"}}
		rs.Spec.Replicas = int32p(3)
		r := &ReplicaSetReconciler{Client: statusClient(t, secret)}

		got := r.observeLeader(ctx, cluster, rs, clusterconfig.Settings{Failover: "election", Port: 3301, SuperUser: "admin"})
		if got != "rs-2" {
			t.Errorf("observeLeader(election) = %q, want rs-2", got)
		}
	})
}

// TestIprotoOpsWithoutResolvablePassword: a super user is introspected from
// config but its password Secret is gone — reload and schema upgrade must
// degrade to no-ops, not fail the reconcile.
func TestIprotoOpsWithoutResolvablePassword(t *testing.T) {
	ctx := context.Background()
	secretless := statusClient(t) // no credentials Secret
	cluster := &v2alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"},
		Spec:       v2alpha1.ClusterSpec{CredentialsSecret: "gone"},
	}
	replicas := int32(1)
	rs := &v2alpha1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "default"},
		Spec: v2alpha1.ReplicaSetSpec{Replicas: &replicas, PodTemplate: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "tarantool", Image: "tarantool/tarantool:3"}}},
		}},
	}
	r := &ReplicaSetReconciler{Client: secretless, Recorder: record.NewFakeRecorder(8)}

	if _, retry, _ := r.reloadConfig(ctx, cluster, rs, superSettings(), "H", rs.GetReplicas()); retry {
		t.Errorf("reloadConfig without a resolvable password = retry %v, want false", retry)
	}
	if r.maybeUpgradeSchema(ctx, cluster, rs, superSettings(), 1) {
		t.Errorf("maybeUpgradeSchema without a resolvable password should be a no-op")
	}
}

func TestNeedsNetworkLeaderObservationUnknownMode(t *testing.T) {
	if !needsNetworkLeaderObservation("future-mode") {
		t.Errorf("an unknown failover mode should assume the leader must be observed")
	}
}

func TestTarantoolImageNoContainers(t *testing.T) {
	// With no containers at all there is no image (one container = the fallback).
	if got := tarantoolImage(&v2alpha1.ReplicaSet{}); got != "" {
		t.Errorf("tarantoolImage with no containers = %q, want \"\"", got)
	}
}

func TestInjectLeaderStepDownNoContainers(t *testing.T) {
	tpl := &corev1.PodTemplateSpec{}
	injectLeaderStepDown(tpl, "election") // must not panic on an empty pod spec
	if len(tpl.Spec.Containers) != 0 {
		t.Errorf("no container should have been invented: %v", tpl.Spec.Containers)
	}
}

// TestRuntimeOpsRequeues: the two requeue sources of the runtime block.
func TestRuntimeOpsRequeues(t *testing.T) {
	ctx := context.Background()

	t.Run("reachable-but-stale instance requeues the reload", func(t *testing.T) {
		orig := evalInstanceString
		t.Cleanup(func() { evalInstanceString = orig })
		evalInstanceString = func(context.Context, string, string, string, string, time.Duration) (string, error) {
			return "OLD", nil // reachable, still on an older config version
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
			Data:       map[string][]byte{"admin": []byte("pw")},
		}
		cluster := &v2alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"},
			Spec:       v2alpha1.ClusterSpec{CredentialsSecret: "creds"},
		}
		replicas := int32(1)
		rs := &v2alpha1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: "rs", Namespace: "default"},
			Spec:       v2alpha1.ReplicaSetSpec{Replicas: &replicas},
		}
		r := &ReplicaSetReconciler{Client: statusClient(t, secret), Recorder: record.NewFakeRecorder(8)}

		settings := clusterconfig.Settings{Failover: "manual", Leader: "rs-0", Port: 3301, SuperUser: "admin"}
		requeue, leader, _ := r.runtimeOps(ctx, cluster, rs, settings, "H", 1, rs.GetReplicas(), false)
		if requeue != configReloadRequeue {
			t.Errorf("requeue = %v, want %v (stale instance must be retried)", requeue, configReloadRequeue)
		}
		if leader != "rs-0" {
			t.Errorf("leader = %q, want rs-0 (declared by manual mode)", leader)
		}
	})

	t.Run("remediation candidate inside the grace window requeues", func(t *testing.T) {
		cl, rs := remRS("election", 3, true)
		c := fakeClientWith(t, deadNode("n1", time.Now()), instancePod("rem-rs", 1, "n1"))
		r := &ReplicaSetReconciler{Client: c, Recorder: record.NewFakeRecorder(8)}
		cluster := &v2alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}}

		requeue, _, _ := r.runtimeOps(ctx, cluster, rs, cl, "H", 2, rs.GetReplicas(), false) // quorum held; no super user, so no iproto ops
		if requeue != remediationRequeue {
			t.Errorf("requeue = %v, want %v (grace-window candidate)", requeue, remediationRequeue)
		}
	})
}

// TestRemediationManualModeProtection: manual failover protects the declared
// leader (or the first instance when none is declared).
func TestRemediationManualModeProtection(t *testing.T) {
	ctx := context.Background()
	past := time.Now().Add(-2 * nodeLostGracePeriod)

	t.Run("declared leader protected", func(t *testing.T) {
		cl, rs := remRS("manual", 2, true)
		cl.Leader = "rem-rs-1"
		c := fakeClientWith(t, deadNode("n1", past),
			instancePod("rem-rs", 0, "n1"), instancePod("rem-rs", 1, "n1"))
		(&ReplicaSetReconciler{Client: c, Recorder: record.NewFakeRecorder(8)}).remediateStuckPods(ctx, cl, rs, 2)
		if !podExists(t, c, "rem-rs-1") {
			t.Errorf("the declared manual leader must never be auto-remediated")
		}
		if podExists(t, c, "rem-rs-0") {
			t.Errorf("the non-leader replica should have been remediated")
		}
	})

	t.Run("no declared leader: first instance protected", func(t *testing.T) {
		cl, rs := remRS("manual", 2, true)
		c := fakeClientWith(t, deadNode("n1", past),
			instancePod("rem-rs", 0, "n1"), instancePod("rem-rs", 1, "n1"))
		(&ReplicaSetReconciler{Client: c, Recorder: record.NewFakeRecorder(8)}).remediateStuckPods(ctx, cl, rs, 2)
		if !podExists(t, c, "rem-rs-0") {
			t.Errorf("the default manual leader (<rs>-0) must never be auto-remediated")
		}
	})
}

// TestPodStuckSinceShapes covers the pod/node shapes podStuckSince classifies.
func TestPodStuckSinceShapes(t *testing.T) {
	ctx := context.Background()

	t.Run("stuck Terminating", func(t *testing.T) {
		pod := instancePod("rem-rs", 0, "n1")
		now := metav1.Now()
		pod.DeletionTimestamp = &now
		if _, stuck := podStuckSince(ctx, statusClient(t), pod); !stuck {
			t.Errorf("a Terminating pod is stuck regardless of its node")
		}
	})

	t.Run("unscheduled pod is not stuck", func(t *testing.T) {
		pod := instancePod("rem-rs", 0, "")
		if _, stuck := podStuckSince(ctx, statusClient(t), pod); stuck {
			t.Errorf("a pod with no node cannot be node-stuck")
		}
	})

	t.Run("node without a Ready condition is inconclusive", func(t *testing.T) {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "bare"}}
		pod := instancePod("rem-rs", 0, "bare")
		if _, stuck := podStuckSince(ctx, statusClient(t, node), pod); stuck {
			t.Errorf("no Ready condition must not be treated as node-lost")
		}
	})
}

// TestPhaseLifecycle drives the real Reconcile through the full phase story
// with a fake client: Configuring during initial convergence (never Degraded at
// startup), Ready once all instances are ready, Degraded — not Configuring —
// when a previously-ready set loses an instance, and back to Ready on recovery.
func TestPhaseLifecycle(t *testing.T) {
	ctx := context.Background()

	replicas := int32(2)
	rs := &v2alpha1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ph-rs", Namespace: "default"},
		Spec: v2alpha1.ReplicaSetSpec{
			ClusterName: "ph", Replicas: &replicas,
			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "tarantool", Image: "tarantool/tarantool:3"}},
			}},
		},
	}
	cluster := &v2alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "ph", Namespace: "default"}}

	// A real rendered config (failover off, no super user — runtimeOps needs no
	// network) so ContainsReplicaSet and IntrospectSettings see the live shape.
	rendered, hash, err := clusterconfig.RenderAndHash(clusterconfig.Input{
		Cluster: cluster, ReplicaSets: []v2alpha1.ReplicaSet{*rs},
	})
	if err != nil {
		t.Fatalf("RenderAndHash: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: ConfigSecretName("ph"), Namespace: "default",
			Annotations: map[string]string{ConfigHashAnnotation: hash},
		},
		Data: map[string][]byte{ConfigFileName: rendered},
	}

	c := fake.NewClientBuilder().
		WithScheme(mappingScheme(t)).
		WithStatusSubresource(&v2alpha1.ReplicaSet{}, &appsv1.StatefulSet{}).
		WithObjects(rs, cluster, secret).
		Build()
	r := &ReplicaSetReconciler{Client: c, Scheme: mappingScheme(t), Recorder: record.NewFakeRecorder(8)}

	phase := func() v2alpha1.ReplicaSetPhase {
		t.Helper()
		if _, err := r.Reconcile(ctx, reqFor("default", "ph-rs")); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		got := &v2alpha1.ReplicaSet{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ph-rs"}, got); err != nil {
			t.Fatalf("get rs: %v", err)
		}
		return got.Status.Phase
	}
	setReady := func(ready int32) {
		t.Helper()
		sts := &appsv1.StatefulSet{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ph-rs"}, sts); err != nil {
			t.Fatalf("get sts: %v", err)
		}
		sts.Status.Replicas = replicas
		sts.Status.ReadyReplicas = ready
		if err := c.Status().Update(ctx, sts); err != nil {
			t.Fatalf("update sts status: %v", err)
		}
	}

	// Initial convergence: 0 then 1 of 2 ready — Configuring, NEVER Degraded.
	if got := phase(); got != v2alpha1.ReplicaSetConfiguring {
		t.Fatalf("startup phase = %q, want Configuring", got)
	}
	setReady(1)
	if got := phase(); got != v2alpha1.ReplicaSetConfiguring {
		t.Fatalf("partial startup phase = %q, want Configuring (not Degraded at startup)", got)
	}

	// Fully ready: Ready, and the sticky Bootstrapped flag is set.
	setReady(2)
	if got := phase(); got != v2alpha1.ReplicaSetReady {
		t.Fatalf("phase = %q, want Ready", got)
	}

	// Regression after health: an instance goes not-ready (the stuck-TX drill) —
	// Degraded, not Configuring.
	setReady(1)
	if got := phase(); got != v2alpha1.ReplicaSetDegraded {
		t.Fatalf("post-bootstrap regression phase = %q, want Degraded", got)
	}

	// Recovery: back to Ready.
	setReady(2)
	if got := phase(); got != v2alpha1.ReplicaSetReady {
		t.Fatalf("recovered phase = %q, want Ready", got)
	}
}

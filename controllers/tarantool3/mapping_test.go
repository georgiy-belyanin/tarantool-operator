package tarantool3

import (
	"context"
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
)

// mappingScheme registers corev1 + v2alpha1 so the fake client can list/get the
// operator's CRDs.
func mappingScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("clientgo scheme: %v", err)
	}
	if err := v2alpha1.AddToScheme(s); err != nil {
		t.Fatalf("v2alpha1 scheme: %v", err)
	}
	return s
}

func fakeClientWith(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(mappingScheme(t)).
		// The real manager registers this index in SetupWithManager; the fake
		// client needs it for mapSecretToClusters' MatchingFields lookup.
		WithIndex(&v2alpha1.Cluster{}, credentialsSecretField, indexClusterByCredentialsSecret).
		WithObjects(objs...).Build()
}

// requestNames returns the sorted "ns/name" of each reconcile.Request, for
// order-independent comparison.
func requestNames(reqs []reconcile.Request) []string {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, r.Namespace+"/"+r.Name)
	}
	sort.Strings(out)
	return out
}

func rsIn(ns, name, clusterName string) *v2alpha1.ReplicaSet {
	return &v2alpha1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v2alpha1.ReplicaSetSpec{ClusterName: clusterName},
	}
}

// --- mapReplicaSetToCluster (free function) ----------------------------------

func TestMapReplicaSetToCluster(t *testing.T) {
	t.Run("maps to its cluster in the same namespace", func(t *testing.T) {
		got := mapReplicaSetToCluster(context.Background(), rsIn("ns1", "storage-001", "example"))
		if want := []string{"ns1/example"}; !equalStrings(requestNames(got), want) {
			t.Errorf("got %v, want %v", requestNames(got), want)
		}
	})
	t.Run("empty clusterName yields no request", func(t *testing.T) {
		if got := mapReplicaSetToCluster(context.Background(), rsIn("ns1", "orphan", "")); len(got) != 0 {
			t.Errorf("got %v, want none", got)
		}
	})
	t.Run("non-ReplicaSet object yields no request", func(t *testing.T) {
		if got := mapReplicaSetToCluster(context.Background(), &corev1.Secret{}); got != nil {
			t.Errorf("got %v, want nil", got)
		}
	})
}

// --- ClusterReconciler.replicaSetsForCluster ---------------------------------

func TestReplicaSetsForCluster(t *testing.T) {
	c := fakeClientWith(t,
		rsIn("ns1", "a", "example"),
		rsIn("ns1", "b", "example"),
		rsIn("ns1", "c", "other"), // different cluster
	)
	r := &ClusterReconciler{Client: c}
	cluster := &v2alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "example"}}

	got, err := r.replicaSetsForCluster(context.Background(), cluster)
	if err != nil {
		t.Fatalf("replicaSetsForCluster: %v", err)
	}
	names := []string{}
	for _, rs := range got {
		names = append(names, rs.Name)
	}
	sort.Strings(names)
	if want := []string{"a", "b"}; !equalStrings(names, want) {
		t.Errorf("got %v, want %v (only this cluster's replicasets)", names, want)
	}
}

// --- ClusterReconciler.mapSecretToClusters -----------------------------------

func clusterWithCredRef(ns, name, secretName string) *v2alpha1.Cluster {
	return &v2alpha1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       v2alpha1.ClusterSpec{CredentialsSecret: secretName},
	}
}

func TestMapSecretToClusters(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "creds"}}

	t.Run("enqueues clusters referencing the secret", func(t *testing.T) {
		c := fakeClientWith(t,
			clusterWithCredRef("ns1", "wants-it", "creds"),
			clusterWithCredRef("ns1", "other-secret", "different"),
			&v2alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "no-creds"}}, // nil credentials
		)
		r := &ClusterReconciler{Client: c}
		got := r.mapSecretToClusters(context.Background(), secret)
		if want := []string{"ns1/wants-it"}; !equalStrings(requestNames(got), want) {
			t.Errorf("got %v, want %v", requestNames(got), want)
		}
	})

	t.Run("a cluster in another namespace is not matched (namespace isolation)", func(t *testing.T) {
		// The secret lives in ns1; a cluster in ns2 referencing a same-named secret
		// reads only its own ns2 secret, so it must not be enqueued for the ns1 one.
		r := &ClusterReconciler{Client: fakeClientWith(t, clusterWithCredRef("ns2", "elsewhere", "creds"))}
		got := r.mapSecretToClusters(context.Background(), secret)
		if len(got) != 0 {
			t.Errorf("got %v, want none (clusters only match secrets in their own namespace)", requestNames(got))
		}
	})
}

// --- ReplicaSetReconciler.mapConfigSecretToReplicaSets -----------------------

func TestMapConfigSecretToReplicaSets(t *testing.T) {
	r := &ReplicaSetReconciler{Client: fakeClientWith(t,
		rsIn("ns1", "storage-001", "example"),
		rsIn("ns1", "storage-002", "example"),
		rsIn("ns1", "other-rs", "other"), // different cluster
	)}

	t.Run("enqueues every replicaset of the labelled cluster", func(t *testing.T) {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns1", Name: "example-config",
			Labels: map[string]string{ClusterLabel: "example"},
		}}
		got := r.mapConfigSecretToReplicaSets(context.Background(), secret)
		want := []string{"ns1/storage-001", "ns1/storage-002"}
		if !equalStrings(requestNames(got), want) {
			t.Errorf("got %v, want %v", requestNames(got), want)
		}
	})

	t.Run("secret without the cluster label yields no request", func(t *testing.T) {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "unrelated"}}
		if got := r.mapConfigSecretToReplicaSets(context.Background(), secret); got != nil {
			t.Errorf("got %v, want nil", requestNames(got))
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

package clusterconfig_test

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// Run `go test ./pkg/clusterconfig/... -update` to (re)generate golden files.
//
// go-config's YAML output is not byte-stable (it preserves the source map's
// random iteration order), so comparisons here are semantic: both sides are
// parsed to a generic structure and deep-compared. The golden files are kept
// as human-readable expected output, not as byte oracles.
var update = flag.Bool("update", false, "update golden files")

func ptr[T any](v T) *T { return &v }

func jsonExt(s string) *apiextensionsv1.JSON {
	return &apiextensionsv1.JSON{Raw: []byte(s)}
}

// dropConfigHashLabel removes the operator-internal labels.operator_config_hash
// entry (and an emptied labels map) so golden comparisons ignore the volatile
// content-hash the renderer stamps for reload verification.
func dropConfigHashLabel(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	if labels, ok := m["labels"].(map[string]any); ok {
		delete(labels, clusterconfig.ConfigHashLabel)
		if len(labels) == 0 {
			delete(m, "labels")
		}
	}
	return m
}

// parseYAML unmarshals YAML into a generic, order-independent structure.
func parseYAML(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshalling YAML: %v\n%s", err, b)
	}
	return v
}

func TestRender(t *testing.T) {
	tests := []struct {
		name   string
		input  clusterconfig.Input
		golden string
	}{
		{
			name:   "minimal-unsharded-failover-off",
			golden: "minimal.golden.yaml",
			input: clusterconfig.Input{
				Cluster: &v2alpha1.Cluster{
					ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default"},
					Spec: v2alpha1.ClusterSpec{
						Domain: "cluster.local",
						Config: jsonExt(`{"replication":{"failover":"off"}}`),
					},
				},
				ReplicaSets: []v2alpha1.ReplicaSet{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "router-001", Namespace: "default"},
						Spec: v2alpha1.ReplicaSetSpec{
							ClusterName: "example",
							Group:       "routers",
							Replicas:    ptr(int32(1)),
						},
					},
				},
			},
		},
		{
			name:   "sharded-election-with-credentials-and-passthrough",
			golden: "sharded.golden.yaml",
			input: clusterconfig.Input{
				Cluster: &v2alpha1.Cluster{
					ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "tarantool"},
					Spec: v2alpha1.ClusterSpec{
						Domain: "cluster.local",
						Config: jsonExt(`{"credentials":{"users":{"replicator":{"roles":["replication"]},"storage":{"roles":["sharding"]}}},"replication":{"failover":"election"},"sharding":{"bucket_count":3000},"log":{"level":5},"memtx":{"memory":268435456}}`),
					},
				},
				Passwords: map[string]string{
					"replicator": "replicator-secret",
					"storage":    "storage-secret",
				},
				ReplicaSets: []v2alpha1.ReplicaSet{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "router-001", Namespace: "tarantool"},
						Spec: v2alpha1.ReplicaSetSpec{
							ClusterName: "example",
							Group:       "routers",
							Replicas:    ptr(int32(1)),
							Config:      jsonExt(`{"sharding":{"roles":["router"]},"roles":["roles.crud-router"]}`),
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{Name: "storage-001", Namespace: "tarantool"},
						Spec: v2alpha1.ReplicaSetSpec{
							ClusterName: "example",
							Group:       "storages",
							Replicas:    ptr(int32(2)),
							Config:      jsonExt(`{"sharding":{"roles":["storage"],"weight":1}}`),
						},
					},
					{
						ObjectMeta: metav1.ObjectMeta{Name: "storage-002", Namespace: "tarantool"},
						Spec: v2alpha1.ReplicaSetSpec{
							ClusterName: "example",
							Group:       "storages",
							Replicas:    ptr(int32(2)),
							Config:      jsonExt(`{"sharding":{"roles":["storage"],"weight":1}}`),
						},
					},
				},
			},
		},
		{
			name:   "manual-failover-with-leader",
			golden: "manual.golden.yaml",
			input: clusterconfig.Input{
				Cluster: &v2alpha1.Cluster{
					ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default"},
					Spec: v2alpha1.ClusterSpec{
						Domain: "cluster.local",
						Config: jsonExt(`{"replication":{"failover":"manual"}}`),
					},
				},
				ReplicaSets: []v2alpha1.ReplicaSet{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "storage-001", Namespace: "default"},
						Spec: v2alpha1.ReplicaSetSpec{
							ClusterName: "example",
							Group:       "storages",
							Replicas:    ptr(int32(2)),
							Config:      jsonExt(`{"leader":"storage-001-0"}`),
						},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := clusterconfig.Render(tc.input)
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}

			path := filepath.Join("testdata", tc.golden)
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatalf("writing golden: %v", err)
				}
			}

			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading golden %s (run with -update to create): %v", path, err)
			}

			// Strip the operator-internal config-version label (a content hash the
			// renderer stamps for reload verification) so the goldens stay stable and
			// human-readable instead of carrying a brittle hash value.
			gotV := dropConfigHashLabel(parseYAML(t, got))
			wantV := dropConfigHashLabel(parseYAML(t, want))
			if !reflect.DeepEqual(gotV, wantV) {
				t.Errorf("rendered config mismatch for %s\n--- got ---\n%s\n--- want (golden) ---\n%s", tc.golden, got, want)
			}
		})
	}
}

// TestRenderSemanticallyStable verifies that, although byte output may reorder
// keys, the rendered configuration is semantically identical across renders.
func TestRenderSemanticallyStable(t *testing.T) {
	in := clusterconfig.Input{
		Cluster: &v2alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default"},
			Spec: v2alpha1.ClusterSpec{
				Config: jsonExt(`{"replication":{"failover":"election"},"sharding":{"bucket_count":3000}}`),
			},
		},
		ReplicaSets: []v2alpha1.ReplicaSet{
			{ObjectMeta: metav1.ObjectMeta{Name: "storage-001"}, Spec: v2alpha1.ReplicaSetSpec{ClusterName: "example", Group: "storages", Replicas: ptr(int32(3)), Config: jsonExt(`{"sharding":{"roles":["storage"]}}`)}},
			{ObjectMeta: metav1.ObjectMeta{Name: "storage-002"}, Spec: v2alpha1.ReplicaSetSpec{ClusterName: "example", Group: "storages", Replicas: ptr(int32(3)), Config: jsonExt(`{"sharding":{"roles":["storage"]}}`)}},
		},
	}

	first, err := clusterconfig.Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	firstV := parseYAML(t, first)
	for i := 0; i < 20; i++ {
		got, err := clusterconfig.Render(in)
		if err != nil {
			t.Fatalf("Render() error = %v", err)
		}
		if !reflect.DeepEqual(parseYAML(t, got), firstV) {
			t.Fatalf("semantically divergent output on iteration %d", i)
		}
	}
}

// TestRenderGroupsSeparateRouterAndStorage verifies that replicasets in
// different groups land under their own groups.<g>.replicasets node and do not
// bleed into each other.
func TestRenderGroupsSeparateRouterAndStorage(t *testing.T) {
	in := clusterconfig.Input{
		Cluster: &v2alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default"},
			Spec:       v2alpha1.ClusterSpec{Config: jsonExt(`{"replication":{"failover":"election"}}`)},
		},
		ReplicaSets: []v2alpha1.ReplicaSet{
			{ObjectMeta: metav1.ObjectMeta{Name: "router-001", Namespace: "default"}, Spec: v2alpha1.ReplicaSetSpec{ClusterName: "example", Group: "routers", Replicas: ptr(int32(1))}},
			{ObjectMeta: metav1.ObjectMeta{Name: "storage-001", Namespace: "default"}, Spec: v2alpha1.ReplicaSetSpec{ClusterName: "example", Group: "storages", Replicas: ptr(int32(1))}},
		},
	}

	tree := renderTree(t, in)
	groups := submap(t, tree, "groups")
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %d: %v", len(groups), groups)
	}
	if _, ok := submap(t, submap(t, groups, "routers"), "replicasets")["router-001"]; !ok {
		t.Errorf("router-001 missing from routers group")
	}
	if _, ok := submap(t, submap(t, groups, "storages"), "replicasets")["storage-001"]; !ok {
		t.Errorf("storage-001 missing from storages group")
	}
	if _, leaked := submap(t, submap(t, groups, "routers"), "replicasets")["storage-001"]; leaked {
		t.Errorf("storage-001 leaked into routers group")
	}
}

// TestRenderTwoReplicaSetsSameGroup verifies both replicasets of one group
// appear under that group's single replicasets map.
func TestRenderTwoReplicaSetsSameGroup(t *testing.T) {
	in := clusterconfig.Input{
		Cluster: &v2alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "default"},
			Spec:       v2alpha1.ClusterSpec{Config: jsonExt(`{"replication":{"failover":"election"}}`)},
		},
		ReplicaSets: []v2alpha1.ReplicaSet{
			{ObjectMeta: metav1.ObjectMeta{Name: "storage-001", Namespace: "default"}, Spec: v2alpha1.ReplicaSetSpec{ClusterName: "example", Group: "storages", Replicas: ptr(int32(1))}},
			{ObjectMeta: metav1.ObjectMeta{Name: "storage-002", Namespace: "default"}, Spec: v2alpha1.ReplicaSetSpec{ClusterName: "example", Group: "storages", Replicas: ptr(int32(1))}},
		},
	}

	rsets := submap(t, submap(t, submap(t, renderTree(t, in), "groups"), "storages"), "replicasets")
	if len(rsets) != 2 {
		t.Fatalf("want 2 replicasets under storages, got %d: %v", len(rsets), rsets)
	}
}

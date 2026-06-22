package clusterconfig_test

import (
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// TestRenderRejectsMalformedConfigJSON: each raw config section is user input;
// a malformed one must fail the render with an error naming the section, not
// panic or render a half-config.
func TestRenderRejectsMalformedConfigJSON(t *testing.T) {
	bad := jsonExt(`{not json`)

	t.Run("cluster spec.config", func(t *testing.T) {
		in := baseInput()
		in.Cluster.Spec.Config = bad
		if _, err := clusterconfig.Render(in); err == nil || !strings.Contains(err.Error(), "spec.config") {
			t.Errorf("Render() error = %v, want a cluster spec.config parse error", err)
		}
	})
	t.Run("cluster groupConfigs", func(t *testing.T) {
		in := baseInput()
		in.Cluster.Spec.GroupConfigs = map[string]apiextensionsv1.JSON{"storages": *bad}
		if _, err := clusterconfig.Render(in); err == nil || !strings.Contains(err.Error(), "group") {
			t.Errorf("Render() error = %v, want a group config parse error", err)
		}
	})
	t.Run("replicaset spec.config", func(t *testing.T) {
		in := baseInput()
		in.ReplicaSets[0].Spec.Config = bad
		if _, err := clusterconfig.Render(in); err == nil || !strings.Contains(err.Error(), "replicaset") {
			t.Errorf("Render() error = %v, want a replicaset config parse error", err)
		}
	})
	t.Run("replicaset instanceConfigs", func(t *testing.T) {
		in := baseInput()
		in.ReplicaSets[0].Spec.InstanceConfigs = map[string]apiextensionsv1.JSON{"0": *bad}
		if _, err := clusterconfig.Render(in); err == nil || !strings.Contains(err.Error(), "instance") {
			t.Errorf("Render() error = %v, want an instance config parse error", err)
		}
	})
	t.Run("empty raw JSON renders fine", func(t *testing.T) {
		in := baseInput()
		in.ReplicaSets[0].Spec.Config = &apiextensionsv1.JSON{}
		if _, err := clusterconfig.Render(in); err != nil {
			t.Errorf("Render() with an empty config section error = %v, want nil", err)
		}
	})
}

// TestClusterPortFallbacks: the Service port helper must fall back to the
// default on a malformed cluster config rather than fail the reconcile.
func TestClusterPortFallbacks(t *testing.T) {
	t.Run("malformed config", func(t *testing.T) {
		c := &v2alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "x"}}
		c.Spec.Config = jsonExt(`{not json`)
		if got := clusterconfig.ClusterPort(c); got != 3301 {
			t.Errorf("ClusterPort = %d, want 3301 fallback", got)
		}
	})
	t.Run("global listen sets the port", func(t *testing.T) {
		c := &v2alpha1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "x"}}
		c.Spec.Config = jsonExt(`{"iproto":{"listen":[{"uri":"0.0.0.0:4500"}]}}`)
		if got := clusterconfig.ClusterPort(c); got != 4500 {
			t.Errorf("ClusterPort = %d, want 4500", got)
		}
	})
}

// TestPortIntrospectionEdgeURIs: listen URIs the port extraction cannot parse
// must yield the conventional default instead of garbage.
func TestPortIntrospectionEdgeURIs(t *testing.T) {
	for name, listen := range map[string]string{
		"uri without a colon":   `[{"uri":"localhost"}]`,
		"non-numeric port":      `[{"uri":"unix/:/var/run/t.sock"}]`,
		"listen entry sans uri": `[{}]`,
	} {
		t.Run(name, func(t *testing.T) {
			in := baseInput()
			in.Cluster.Spec.Config = jsonExt(`{"iproto":{"listen":` + listen + `}}`)
			if s := introspect(t, in); s.Port != 3301 {
				t.Errorf("Port = %d, want 3301 default for %s", s.Port, name)
			}
		})
	}
}

// TestNumberNormalization: the JSON decoding keeps integers integral (not
// 2.68435456e+08), renders non-integral numbers as floats, and survives a
// number too large for float64.
func TestNumberNormalization(t *testing.T) {
	in := baseInput()
	in.Cluster.Spec.Config = jsonExt(`{"memtx":{"memory":268435456},"app":{"cfg":{"weights":[0.5,2],"huge":1e999999}}}`)

	tree := renderTree(t, in)
	if got := submap(t, tree, "memtx")["memory"]; got != float64(268435456) {
		t.Errorf("memtx.memory = %v (%T), want 268435456", got, got)
	}
	cfg := submap(t, submap(t, tree, "app"), "cfg")
	weights, _ := cfg["weights"].([]any)
	if len(weights) != 2 || weights[0] != 0.5 || weights[1] != float64(2) {
		t.Errorf("weights = %v, want [0.5 2]", cfg["weights"])
	}
	if huge, ok := cfg["huge"].(string); !ok || huge != "1e999999" {
		t.Errorf("a number beyond float64 should pass through as its literal string, got %v (%T)", cfg["huge"], cfg["huge"])
	}
}

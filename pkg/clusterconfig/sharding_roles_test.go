package clusterconfig_test

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
)

// TestShardingRoleDetection: IsShardStorage / IsShardRouter read sharding.roles
// from the delivered config — the operator keys drain-gated deletion off the
// former and vshard bootstrap off the latter.
func TestShardingRoleDetection(t *testing.T) {
	in := baseInput() // group "storages", replica set "storage-001"
	in.ReplicaSets[0].Spec.Config = jsonExt(`{"sharding":{"roles":["storage"]}}`)
	in.ReplicaSets = append(in.ReplicaSets, v2alpha1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "router-001", Namespace: "default"},
		Spec: v2alpha1.ReplicaSetSpec{
			ClusterName: "example", Group: "routers", Replicas: ptr(int32(1)),
			Config: jsonExt(`{"sharding":{"roles":["router"]}}`),
		},
	})
	d := delivered(t, in)

	if !d.IsShardStorage("storages", "storage-001") {
		t.Errorf("storage-001 should be a shard storage")
	}
	if d.IsShardRouter("storages", "storage-001") {
		t.Errorf("storage-001 is not a router")
	}
	if !d.IsShardRouter("routers", "router-001") {
		t.Errorf("router-001 should be a shard router")
	}
	if d.IsShardStorage("routers", "router-001") {
		t.Errorf("router-001 is not a storage")
	}
}

package tarantool3

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

func deadNode(name string, since time.Time) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type: corev1.NodeReady, Status: corev1.ConditionUnknown,
			LastTransitionTime: metav1.NewTime(since),
		}}},
	}
}

func instancePod(rsName string, ordinal int, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: rsName + "-" + string(rune('0'+ordinal)), Namespace: "default",
			Labels: map[string]string{ReplicaSetLabel: rsName},
		},
		Spec: corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "t", Image: "i"}}},
	}
}

func remRS(failover string, replicas int32, bootstrapped bool) (clusterconfig.Settings, *v2alpha1.ReplicaSet) {
	cl := clusterconfig.Settings{Failover: failover, Port: 3301}
	rs := &v2alpha1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "rem-rs", Namespace: "default"},
		Spec:       v2alpha1.ReplicaSetSpec{ClusterName: "c", Replicas: &replicas},
	}
	rs.Status.Bootstrapped = bootstrapped
	return cl, rs
}

func podExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &corev1.Pod{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("get pod: %v", err)
	}
	return err == nil
}

func TestRemediateStuckPods(t *testing.T) {
	ctx := context.Background()
	past := time.Now().Add(-2 * nodeLostGracePeriod)

	t.Run("stuck pod on dead node past grace is force-deleted", func(t *testing.T) {
		cl, rs := remRS("election", 3, true)
		c := fakeClientWith(t, deadNode("n1", past), instancePod("rem-rs", 1, "n1"))
		r := &ReplicaSetReconciler{Client: c, Recorder: record.NewFakeRecorder(8)}
		if pending := r.remediateStuckPods(ctx, cl, rs, 2); pending {
			t.Errorf("pending = true, want false (already remediated)")
		}
		if podExists(t, c, "rem-rs-1") {
			t.Errorf("stuck pod should have been force-deleted")
		}
	})

	t.Run("within grace: kept, pending requeue", func(t *testing.T) {
		cl, rs := remRS("election", 3, true)
		c := fakeClientWith(t, deadNode("n1", time.Now()), instancePod("rem-rs", 1, "n1"))
		r := &ReplicaSetReconciler{Client: c, Recorder: record.NewFakeRecorder(8)}
		if pending := r.remediateStuckPods(ctx, cl, rs, 2); !pending {
			t.Errorf("pending = false, want true (grace not elapsed)")
		}
		if !podExists(t, c, "rem-rs-1") {
			t.Errorf("pod must not be deleted before the grace period")
		}
	})

	t.Run("quorum not held: no remediation", func(t *testing.T) {
		cl, rs := remRS("election", 3, true)
		c := fakeClientWith(t, deadNode("n1", past), instancePod("rem-rs", 1, "n1"))
		r := &ReplicaSetReconciler{Client: c, Recorder: record.NewFakeRecorder(8)}
		r.remediateStuckPods(ctx, cl, rs, 1) // 1 < quorum(2)
		if !podExists(t, c, "rem-rs-1") {
			t.Errorf("must not remediate when the rest of the set lacks a majority")
		}
	})

	t.Run("not bootstrapped or single instance: no remediation", func(t *testing.T) {
		cl, rs := remRS("election", 3, false)
		c := fakeClientWith(t, deadNode("n1", past), instancePod("rem-rs", 1, "n1"))
		(&ReplicaSetReconciler{Client: c, Recorder: record.NewFakeRecorder(8)}).remediateStuckPods(ctx, cl, rs, 2)
		if !podExists(t, c, "rem-rs-1") {
			t.Errorf("must not remediate a never-bootstrapped set")
		}
	})

	t.Run("off mode protects the writable instance", func(t *testing.T) {
		cl, rs := remRS("off", 2, true)
		c := fakeClientWith(t, deadNode("n1", past),
			instancePod("rem-rs", 0, "n1"), // <rs>-0 is the rw instance in off mode
			instancePod("rem-rs", 1, "n1"))
		(&ReplicaSetReconciler{Client: c, Recorder: record.NewFakeRecorder(8)}).remediateStuckPods(ctx, cl, rs, 2)
		if !podExists(t, c, "rem-rs-0") {
			t.Errorf("the declared writable instance must never be auto-remediated")
		}
		if podExists(t, c, "rem-rs-1") {
			t.Errorf("the replica should have been remediated")
		}
	})

	t.Run("healthy node: untouched", func(t *testing.T) {
		cl, rs := remRS("election", 3, true)
		healthy := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "n1"},
			Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
		}
		c := fakeClientWith(t, healthy, instancePod("rem-rs", 1, "n1"))
		(&ReplicaSetReconciler{Client: c, Recorder: record.NewFakeRecorder(8)}).remediateStuckPods(ctx, cl, rs, 3)
		if !podExists(t, c, "rem-rs-1") {
			t.Errorf("pod on a healthy node must not be touched")
		}
	})

	t.Run("node object gone entirely: pod remediated", func(t *testing.T) {
		cl, rs := remRS("election", 3, true)
		pod := instancePod("rem-rs", 1, "vanished-node")
		pod.CreationTimestamp = metav1.NewTime(past)
		c := fakeClientWith(t, pod)
		(&ReplicaSetReconciler{Client: c, Recorder: record.NewFakeRecorder(8)}).remediateStuckPods(ctx, cl, rs, 2)
		if podExists(t, c, "rem-rs-1") {
			t.Errorf("pod on a deleted node should be remediated")
		}
	})
}

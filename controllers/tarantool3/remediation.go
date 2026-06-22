package tarantool3

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// Dead-node remediation. A StatefulSet never replaces a pod on a lost node: the
// kubelet can't confirm the pod stopped, so it sits NotReady (then Terminating)
// until a human force-deletes it. The reconciler force-deletes instance pods
// whose node is unreachable (or that are stuck Terminating) past a grace period,
// letting the StatefulSet reschedule; the replacement rejoins from the leader.
//
// Safety gates (force-delete frees the pod identity while the old container may
// still be running on a partitioned node, so guard against split-brain):
//   - only multi-instance, already-bootstrapped sets (a rebuilt replica re-syncs
//     from the leader; never the only copy of the data);
//   - a majority of the set must stay ready, so a partitioned remnant can't win
//     election or serve quorum writes;
//   - in off/manual failover the DECLARED writable instance is never remediated
//     automatically — there is no automatic failover to recover from getting it
//     wrong, so that is left to a human.
const (
	// nodeLostGracePeriod is how long a node must be unreachable (or a pod stuck
	// Terminating) before its instance pod is force-deleted.
	nodeLostGracePeriod = 60 * time.Second
	// remediationRequeue re-checks pending candidates before the grace expires.
	remediationRequeue = 30 * time.Second
)

//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete

// remediateStuckPods force-deletes this replica set's instance pods that are
// stranded on unreachable nodes (or stuck Terminating) past the grace period.
// Returns true when there is a candidate that has not yet passed the grace, so
// the caller requeues to re-check.
func (r *ReplicaSetReconciler) remediateStuckPods(ctx context.Context, settings clusterconfig.Settings, rs *v2alpha1.ReplicaSet, ready int32) bool {
	logger := log.FromContext(ctx)

	if !rs.Status.Bootstrapped || rs.GetReplicas() < 2 {
		return false
	}
	// Majority of the set must be ready: a partitioned remnant then cannot hold
	// quorum, and the set survives losing the stuck instance.
	if ready < rs.GetReplicas()/2+1 {
		return false
	}

	// The declared writable instance (off/manual) is never auto-remediated.
	protected := ""
	switch settings.Failover {
	case clusterconfig.FailoverOff:
		if protected = settings.OffModeRW; protected == "" {
			protected = rs.InstanceName(0)
		}
	case clusterconfig.FailoverManual:
		if protected = settings.Leader; protected == "" {
			protected = rs.InstanceName(0)
		}
	case clusterconfig.FailoverElection, clusterconfig.FailoverSupervised:
		// Raft/coordinator fencing handles a partitioned old leader.
	}

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(rs.Namespace),
		client.MatchingLabels{ReplicaSetLabel: rs.Name}); err != nil {
		return false
	}

	now := time.Now()
	pending := false
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Name == protected {
			continue
		}
		stuckSince, stuck := podStuckSince(ctx, r.Client, pod)
		if !stuck {
			continue
		}
		if now.Sub(stuckSince) < nodeLostGracePeriod {
			pending = true // candidate inside the grace window; re-check soon
			continue
		}

		// Force-delete: zero grace + UID precondition so we never delete a
		// recreated successor with the same name.
		zero := int64(0)
		err := r.Delete(ctx, pod, &client.DeleteOptions{
			GracePeriodSeconds: &zero,
			Preconditions:      &metav1.Preconditions{UID: &pod.UID},
		})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			logger.Error(err, "force-deleting stuck instance pod", "pod", pod.Name)
			continue
		}
		logger.Info("force-deleted instance pod stranded on an unreachable node",
			"pod", pod.Name, "node", pod.Spec.NodeName)
		r.Recorder.Eventf(rs, corev1.EventTypeWarning, "StuckPodRemediated",
			"force-deleted instance pod %s stranded on unreachable node %s; the StatefulSet will recreate it and it will rejoin from the leader",
			pod.Name, pod.Spec.NodeName)
	}
	return pending
}

// podStuckSince reports whether the pod is stranded — stuck Terminating, or
// running on a node that is unreachable/gone — and since when.
func podStuckSince(ctx context.Context, c client.Client, pod *corev1.Pod) (time.Time, bool) {
	// Stuck Terminating: deletion requested (taint eviction or a user delete) but
	// the kubelet never confirmed.
	if pod.DeletionTimestamp != nil {
		return pod.DeletionTimestamp.Time, true
	}
	if pod.Spec.NodeName == "" {
		return time.Time{}, false
	}
	node := &corev1.Node{}
	if err := c.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, node); err != nil {
		if apierrors.IsNotFound(err) {
			// Node object gone entirely: k8s guarantees its pods are not running.
			return pod.CreationTimestamp.Time, true
		}
		return time.Time{}, false
	}
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			if cond.Status == corev1.ConditionTrue {
				return time.Time{}, false
			}
			// NotReady or Unknown (kubelet unreachable) since the transition.
			return cond.LastTransitionTime.Time, true
		}
	}
	return time.Time{}, false
}

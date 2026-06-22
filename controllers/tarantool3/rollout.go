package tarantool3

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// Leader-aware rollouts. A StatefulSet's own RollingUpdate restarts pods highest
// ordinal first, oblivious to which instance leads — so a leader at a middle
// ordinal restarts mid-roll and hands leadership to a not-yet-updated instance
// that then restarts too: two leadership transitions where one suffices. When the
// operator can observe the leader (election/supervised + a super user) it owns the
// roll instead: the StatefulSet uses OnDelete and the operator deletes stale pods
// one at a time, followers first and the leader strictly last, waiting for full
// readiness between steps. With the election preStop step-down, a rollout then
// costs exactly one leadership transition, onto an instance already on the new
// revision.

// rolloutRequeue is how soon to re-check an in-progress operator-managed roll
// (pod recreation/readiness is not watched by this controller).
const rolloutRequeue = 10 * time.Second

// operatorManagedRollout reports whether the operator orchestrates this
// replica set's rollouts itself. An explicit user-set updateStrategy always
// wins (the user owns the roll); without a super user the leader cannot be
// observed; in off/manual failover there is no election to order around, so
// the StatefulSet's native RollingUpdate is left in charge.
func operatorManagedRollout(rs *v2alpha1.ReplicaSet, settings clusterconfig.Settings) bool {
	if rs.Spec.UpdateStrategy.Type != "" {
		return false
	}
	if settings.SuperUser == "" {
		return false
	}
	return settings.Failover == clusterconfig.FailoverElection ||
		settings.Failover == clusterconfig.FailoverSupervised
}

// orchestrateRollout advances an operator-managed roll by at most one pod:
// it deletes the highest-ordinal pod that is not on the StatefulSet's update
// revision, deferring the observed leader until it is the only stale pod
// left. Steps are taken only with every instance ready (one instance down at
// a time, matching the PDB), so a replacement that fails to come up halts the
// roll instead of cascading. Returns whether a roll is in progress (the
// caller requeues to drive the next step).
func (r *ReplicaSetReconciler) orchestrateRollout(ctx context.Context, rs *v2alpha1.ReplicaSet, leader string) bool {
	logger := log.FromContext(ctx)

	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: rs.Name}, sts); err != nil {
		return false
	}
	if sts.Status.UpdateRevision == "" {
		return false
	}

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(rs.Namespace),
		client.MatchingLabels{ReplicaSetLabel: rs.Name}); err != nil {
		return false
	}

	var stale []*corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			return true // a step is already in flight; wait for it
		}
		if pod.Labels[appsv1.ControllerRevisionHashLabelKey] != sts.Status.UpdateRevision {
			stale = append(stale, pod)
		}
	}
	if len(stale) == 0 {
		return false // converged
	}

	// One instance down at a time: every instance must be back and ready
	// before the next step (a failing new revision halts the roll here).
	if sts.Status.ReadyReplicas != rs.GetReplicas() {
		return true
	}

	// Followers first, highest ordinal first (mirroring the StatefulSet's own
	// order); the leader only when nothing else is stale.
	sort.Slice(stale, func(i, j int) bool {
		return podOrdinal(stale[i].Name) > podOrdinal(stale[j].Name)
	})
	victim := stale[0]
	if len(stale) > 1 && leader != "" {
		for _, pod := range stale {
			if pod.Name != leader {
				victim = pod
				break
			}
		}
	}

	err := r.Delete(ctx, victim, &client.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &victim.UID},
	})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		logger.Error(err, "rolling out instance pod", "pod", victim.Name)
		return true
	}
	role := "follower"
	if victim.Name == leader {
		role = "leader"
	}
	logger.Info("leader-aware rollout: restarting instance", "pod", victim.Name, "role", role, "stale", len(stale))
	r.Recorder.Eventf(rs, corev1.EventTypeNormal, "RollingOut",
		"restarting %s %s on the new revision (%d instance(s) remaining; leader restarts last)",
		role, victim.Name, len(stale))
	return true
}

// podOrdinal extracts the StatefulSet ordinal from a pod name (<set>-<n>).
func podOrdinal(name string) int {
	idx := strings.LastIndex(name, "-")
	if idx < 0 {
		return -1
	}
	n, err := strconv.Atoi(name[idx+1:])
	if err != nil {
		return -1
	}
	return n
}

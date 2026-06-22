package tarantool3

import (
	"context"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// ShardDrainFinalizer gates deletion of a vshard storage replica set until its
// buckets have drained, so removing a shard never loses data.
const ShardDrainFinalizer = "tarantool.io/drain-buckets"

// shardDrainRequeue is how often to re-check drain progress while a storage
// replica set is being deleted.
const shardDrainRequeue = 10 * time.Second

// bucketCountEvalScript returns (as a string) the number of buckets a storage
// instance still owns or is moving — active + sending + receiving + pinned.
// Zero means the instance holds no data and is safe to remove. A non-vshard
// instance (role not loaded) returns 0.
const bucketCountEvalScript = `
local ok, vshard = pcall(require, 'vshard')
if not ok then return '0' end
local info = vshard.storage.info()
if info == nil or info.bucket == nil then return '0' end
local b = info.bucket
return tostring((b.active or 0) + (b.sending or 0) + (b.receiving or 0) + (b.pinned or 0))
`

// reconcileDelete drives safe removal of a storage replica set. The finalizer is
// only present on a vshard storage the operator can reach over iproto; while it
// is held, the renderer delivers sharding.weight 0 for this set, so vshard
// drains its buckets onto the remaining storages. This handler pushes that
// weight-0 config live (config:reload) and removes the finalizer only once the
// set owns no buckets — at which point the StatefulSet is garbage-collected.
func (r *ReplicaSetReconciler) reconcileDelete(ctx context.Context, rs *v2alpha1.ReplicaSet) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(rs, ShardDrainFinalizer) {
		return ctrl.Result{}, nil // not drain-gated; plain GC
	}

	// If the owning Cluster or its config Secret is already gone (whole-cluster
	// teardown), there is nothing to drain onto — release the finalizer.
	cluster := &v2alpha1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: rs.Spec.ClusterName}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.releaseShardFinalizer(ctx, rs)
		}
		return ctrl.Result{}, err
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: ConfigSecretName(cluster.Name)}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.releaseShardFinalizer(ctx, rs)
		}
		return ctrl.Result{}, err
	}
	delivered, err := clusterconfig.ParseDelivered(secret.Data[ConfigFileName])
	if err != nil {
		return ctrl.Result{}, err
	}
	settings := delivered.Settings(rs.GetGroup(), rs.Name)
	if settings.SuperUser == "" {
		// No way to verify the drain anymore (the super user was removed); do not
		// block deletion indefinitely.
		return ctrl.Result{}, r.releaseShardFinalizer(ctx, rs)
	}

	// Push the weight-0 config (the renderer set it for this deleting set) to the
	// instances so vshard starts draining; sharding.weight is a dynamic key.
	r.reloadConfig(ctx, cluster, rs, settings, secret.Annotations[ConfigHashAnnotation], rs.GetReplicas())

	remaining, ok := r.replicaSetBuckets(ctx, cluster, rs, settings)
	if ok && remaining == 0 {
		r.Recorder.Eventf(rs, corev1.EventTypeNormal, "ShardDrained",
			"storage replica set %s drained all buckets; removing it", rs.Name)
		return ctrl.Result{}, r.releaseShardFinalizer(ctx, rs)
	}
	// Still draining (or the count could not be confirmed yet) — keep the
	// finalizer and re-check. Blocking on an unconfirmed count is deliberate: it
	// is safer to wait (an operator can remove the finalizer by hand if a set is
	// truly stuck) than to delete a storage that may still own buckets.
	logger.Info("draining buckets before deleting storage replica set",
		"replicaset", rs.Name, "remaining", remaining, "confirmed", ok)
	return ctrl.Result{RequeueAfter: shardDrainRequeue}, nil
}

// releaseShardFinalizer removes the drain finalizer, allowing the API server to
// finish deleting the replica set (and garbage-collect its StatefulSet).
func (r *ReplicaSetReconciler) releaseShardFinalizer(ctx context.Context, rs *v2alpha1.ReplicaSet) error {
	if !controllerutil.ContainsFinalizer(rs, ShardDrainFinalizer) {
		return nil
	}
	patch := client.MergeFrom(rs.DeepCopy())
	controllerutil.RemoveFinalizer(rs, ShardDrainFinalizer)
	return r.Patch(ctx, rs, patch)
}

// replicaSetBuckets returns the number of buckets the replica set still owns,
// read over iproto from the first reachable instance (all instances of a storage
// share the same _bucket). ok is false when no instance answered.
func (r *ReplicaSetReconciler) replicaSetBuckets(ctx context.Context, cluster *v2alpha1.Cluster, rs *v2alpha1.ReplicaSet, settings clusterconfig.Settings) (count int, ok bool) {
	password, err := resolveUserPassword(ctx, r.Client, cluster, settings.SuperUser)
	if err != nil {
		return 0, false
	}
	forEachInstance(ctx, cluster, rs, settings.Port, rs.GetReplicas(), observeBudget, func(ctx context.Context, addr string) bool {
		out, err := evalInstanceString(ctx, addr, settings.SuperUser, password, bucketCountEvalScript, connectTimeout)
		if err != nil {
			return false // unreachable; try the next instance
		}
		n, perr := strconv.Atoi(strings.TrimSpace(out))
		if perr != nil {
			return false
		}
		count, ok = n, true
		return true // all instances share _bucket; the first answer is authoritative
	})
	return count, ok
}

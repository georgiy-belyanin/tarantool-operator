package tarantool3

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// Version-upgrade orchestration. After a binary upgrade rolls
// through a replica set (image change -> StatefulSet rollout), the system schema
// must be upgraded by running box.schema.upgrade() on the writable leader — but
// only once EVERY instance runs the new binary (a mixed-version set must keep the
// old schema so old binaries can still replicate). box.schema.upgrade() is
// idempotent (a no-op at the latest schema), so running it after every completed
// image change — including the first bootstrap — is safe.
//
// The operator tracks the tarantool container image it last upgraded the schema
// for in Status.SchemaUpgradedForImage and re-runs the step when the image
// changes, gated on: the set fully ready, a "super" credential user (needed for
// DDL over iproto), homogeneous box.info.version across all instances, and a
// reachable writable leader.

// schemaUpgradeTimeout bounds the box.schema.upgrade() call itself (schema DDL
// can take longer than a plain status read).
const schemaUpgradeTimeout = 30 * time.Second

// versionEvalScript returns the instance's binary version.
const versionEvalScript = `return box.info.version`

// schemaUpgradeEvalScript runs the schema upgrade on a writable instance; on a
// read-only instance it is a no-op returning "".
const schemaUpgradeEvalScript = `
if box.info.ro then return "" end
box.schema.upgrade()
return box.info.version
`

// maybeUpgradeSchema runs box.schema.upgrade() on the leader after a completed
// image rollout. Returns true when the schema-upgrade state changed (so the
// caller's status write persists it). Best-effort: any failure leaves the state
// unchanged and the next reconcile retries.
func (r *ReplicaSetReconciler) maybeUpgradeSchema(ctx context.Context, cluster *v2alpha1.Cluster, rs *v2alpha1.ReplicaSet, settings clusterconfig.Settings, ready int32) bool {
	logger := log.FromContext(ctx)

	image := tarantoolImage(rs)
	if image == "" || rs.Status.SchemaUpgradedForImage == image {
		return false
	}
	if ready != rs.GetReplicas() {
		return false // rollout still in progress
	}
	user := settings.SuperUser
	if user == "" {
		return false // cannot run DDL over iproto without a privileged user
	}
	password, err := resolveUserPassword(ctx, r.Client, cluster, user)
	if err != nil {
		return false
	}

	// All instances must run the same binary before the schema may move.
	version, homogeneous := "", true
	forEachInstance(ctx, cluster, rs, settings.Port, rs.GetReplicas(), observeBudget, func(ctx context.Context, addr string) bool {
		v, err := evalInstanceString(ctx, addr, user, password, versionEvalScript, connectTimeout)
		if err != nil || v == "" {
			homogeneous = false // someone unreachable; retry next reconcile
			return true
		}
		if version == "" {
			version = v
			return false
		}
		if v != version {
			logger.Info("schema upgrade deferred: mixed binary versions in replica set",
				"replicaset", rs.Name, "versions", []string{version, v})
			homogeneous = false
			return true
		}
		return false
	})
	if !homogeneous || version == "" {
		return false
	}

	// Run the upgrade on the writable leader (no-op script on read-only peers).
	upgraded := false
	forEachInstance(ctx, cluster, rs, settings.Port, rs.GetReplicas(), observeBudget+schemaUpgradeTimeout, func(ctx context.Context, addr string) bool {
		v, err := evalInstanceString(ctx, addr, user, password, schemaUpgradeEvalScript, schemaUpgradeTimeout)
		if err != nil || v == "" {
			return false // unreachable or read-only peer; keep looking for the leader
		}
		rs.Status.SchemaUpgradedForImage = image
		logger.Info("schema upgraded after image rollout",
			"replicaset", rs.Name, "image", image, "tarantool", v)
		r.Recorder.Eventf(rs, corev1.EventTypeNormal, "SchemaUpgraded",
			"box.schema.upgrade() completed on the leader after rolling out image %s (tarantool %s)", image, v)
		upgraded = true
		return true
	})
	return upgraded
}

// tarantoolImage returns the image of the replica set's tarantool container.
func tarantoolImage(rs *v2alpha1.ReplicaSet) string {
	idx := tarantoolContainerIndex(rs.Spec.PodTemplate.Spec.Containers)
	if idx < 0 {
		return ""
	}
	return rs.Spec.PodTemplate.Spec.Containers[idx].Image
}

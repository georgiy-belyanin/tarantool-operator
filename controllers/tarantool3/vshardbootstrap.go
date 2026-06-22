package tarantool3

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// vshard bootstrap orchestration. A fresh sharded cluster has no buckets until
// vshard is bootstrapped once (vshard.router.bootstrap distributes bucket_count
// buckets across the storages). Nothing in the declarative config does this, so
// without operator help a sharded cluster comes up with zero buckets and cannot
// serve sharded requests until someone runs bootstrap by hand.
//
// The operator does it through a router replica set, gated on a "super"
// credential user (iproto). vshard.router.bootstrap({if_not_bootstrapped=true})
// is idempotent — a no-op once bootstrapped — and fails (retry) while any
// storage is still unreachable, so calling it best-effort and retrying is safe:
// it self-gates until the storages are up, then distributes evenly; vshard's
// rebalancer handles storages that join afterwards. Success is recorded in the
// sticky Status.ShardingBootstrapped so the operator stops re-issuing it.

// shardBootstrapRequeue is how soon to retry vshard bootstrap while the storages
// are not yet all reachable.
const shardBootstrapRequeue = 10 * time.Second

// vshardBootstrapEvalScript runs an idempotent vshard bootstrap on a router and
// reports the outcome as a single token: "ok" (bootstrapped or already so),
// "retry" (storages not yet reachable), or "novshard" (module absent).
const vshardBootstrapEvalScript = `
local ok, vshard = pcall(require, 'vshard')
if not ok or vshard.router == nil then return 'novshard' end
local res, err = vshard.router.bootstrap({if_not_bootstrapped = true})
if res then return 'ok' end
return 'retry: ' .. tostring(err)
`

// maybeBootstrapVshard bootstraps vshard through this router replica set when it
// has not been bootstrapped yet. Best-effort and idempotent. It returns retry=true
// only when the storages are not all reachable yet and the caller should requeue;
// false otherwise — bootstrapped this pass (the sticky flag is set here so
// applyStatus persists it), already bootstrapped, or not applicable. Gated on a
// super user and at least one ready router instance.
func (r *ReplicaSetReconciler) maybeBootstrapVshard(ctx context.Context, cluster *v2alpha1.Cluster, rs *v2alpha1.ReplicaSet, settings clusterconfig.Settings, ready int32) (retry bool) {
	logger := log.FromContext(ctx)

	if rs.Status.ShardingBootstrapped || ready == 0 {
		return false
	}
	user := settings.SuperUser
	if user == "" {
		return false // cannot reach iproto to bootstrap
	}
	password, err := resolveUserPassword(ctx, r.Client, cluster, user)
	if err != nil {
		return false
	}

	forEachInstance(ctx, cluster, rs, settings.Port, rs.GetReplicas(), observeBudget, func(ctx context.Context, addr string) bool {
		out, err := evalInstanceString(ctx, addr, user, password, vshardBootstrapEvalScript, connectTimeout)
		if err != nil {
			return false // router instance unreachable; try the next one
		}
		switch {
		case out == "ok":
			rs.Status.ShardingBootstrapped = true
			logger.Info("vshard bootstrapped via router", "replicaset", rs.Name)
			r.Recorder.Eventf(rs, corev1.EventTypeNormal, "VshardBootstrapped",
				"vshard.router.bootstrap() distributed the buckets across the storages")
			return true
		case out == "novshard":
			return true // not actually a vshard build; nothing to do
		case strings.HasPrefix(out, "retry"):
			retry = true // storages not all up yet; retry next reconcile
			return true
		}
		return false
	})
	return retry
}

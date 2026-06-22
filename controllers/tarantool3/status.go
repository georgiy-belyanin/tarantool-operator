package tarantool3

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// Condition types set on Cluster and ReplicaSet status (the Deployment-style
// triad plus Ready), so tooling can distinguish "converging" from "degraded but
// serving".
const (
	ConditionReady = "Ready"
	// ConditionAvailable (ReplicaSet): enough instances are ready to serve —
	// at least a majority (⌊n/2⌋+1), the quorum bar for election sets and a
	// conservative availability bar for the other modes.
	ConditionAvailable = "Available"
	// ConditionProgressing: the resource is converging toward the desired state
	// (instances still coming up / config being delivered).
	ConditionProgressing = "Progressing"
	// ConditionDegraded: previously-achieved health regressed — for a ReplicaSet,
	// it had fully formed (Status.Bootstrapped) but has since dropped below a
	// majority; for a Cluster, the render/delivery is in Error.
	ConditionDegraded = "Degraded"
	// ConditionMembersAvailable (Cluster): every ReplicaSet of the cluster
	// reports Available (a quorum of ready instances). The Cluster's own Ready
	// only covers render+delivery; this is the data-plane view, so a cluster
	// whose pods are down does not look healthy at the top-level object.
	ConditionMembersAvailable = "MembersAvailable"
)

// condition builds a metav1.Condition with the observed generation.
func condition(condType string, ok bool, reason string, generation int64) metav1.Condition {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	return metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		ObservedGeneration: generation,
	}
}

// membersAvailableCondition aggregates the children's Available conditions:
// True when every ReplicaSet of the cluster reports a quorum of ready
// instances; the message carries the available/total counts.
func membersAvailableCondition(replicaSets []v2alpha1.ReplicaSet, generation int64) metav1.Condition {
	available := 0
	for i := range replicaSets {
		if meta.IsStatusConditionTrue(replicaSets[i].Status.Conditions, ConditionAvailable) {
			available++
		}
	}

	cond := condition(ConditionMembersAvailable,
		len(replicaSets) > 0 && available == len(replicaSets), "AllReplicaSetsAvailable", generation)
	cond.Message = fmt.Sprintf("%d/%d replica sets have a quorum of ready instances", available, len(replicaSets))
	switch {
	case len(replicaSets) == 0:
		cond.Reason = "NoReplicaSets"
	case available < len(replicaSets):
		cond.Reason = "ReplicaSetUnavailable"
	}
	return cond
}

// replicaSetConditions computes the Available/Progressing/Degraded triad for a
// replica set from its desired/ready counts and the sticky Bootstrapped flag.
func replicaSetConditions(rs *v2alpha1.ReplicaSet, ready int32) []metav1.Condition {
	desired := rs.GetReplicas()
	quorum := desired/2 + 1
	gen := rs.Generation

	available := condition(ConditionAvailable, ready >= quorum, "QuorumUnavailable", gen)
	if ready >= quorum {
		available.Reason = "QuorumReady"
	}
	progressing := condition(ConditionProgressing, ready != desired, "Converged", gen)
	if ready != desired {
		progressing.Reason = "Converging"
	}
	degraded := condition(ConditionDegraded, rs.Status.Bootstrapped && ready < quorum, "Healthy", gen)
	if degraded.Status == metav1.ConditionTrue {
		degraded.Reason = "QuorumLost"
	}
	return []metav1.Condition{available, progressing, degraded}
}

// leaderEvalScript returns the name of the currently elected leader instance, or
// "" when there is no elected leader yet (or this is not an election cluster).
// box.info.replication is keyed by replica id; election.leader is that id.
const leaderEvalScript = `
local e = box.info.election
if e == nil or e.leader == nil or e.leader == 0 then return "" end
local r = box.info.replication[e.leader]
if r == nil then return "" end
return r.name or ""
`

// observeLeader determines the writable leader of a replicaset. In manual
// failover mode the leader is whatever the spec designates (no network needed).
// Otherwise it reads box.info over iproto (go-tarantool) from a reachable
// instance; any failure yields "" so leader observation never fails a reconcile.
func (r *ReplicaSetReconciler) observeLeader(ctx context.Context, cluster *v2alpha1.Cluster, rs *v2alpha1.ReplicaSet, settings clusterconfig.Settings) string {
	// For the non-election modes the operator declares the writable instance, so
	// the leader is known without a network call (this also gives off mode a
	// non-empty status.leader).
	switch settings.Failover {
	case clusterconfig.FailoverManual:
		if settings.Leader != "" {
			return settings.Leader
		}
		return rs.InstanceName(0)
	case clusterconfig.FailoverOff:
		// The user may have designated a different writable instance via
		// instanceConfigs; ordinal 0 is only the renderer's default.
		if settings.OffModeRW != "" {
			return settings.OffModeRW
		}
		return rs.InstanceName(0)
	case clusterconfig.FailoverElection, clusterconfig.FailoverSupervised:
		// Tarantool elects the leader; read it from a running instance below.
	}

	// Reading box.info requires a user with execute access; use the config's
	// first "super" user. Without one we cannot observe the leader.
	user := settings.SuperUser
	if user == "" {
		return ""
	}
	password, err := resolveUserPassword(ctx, r.Client, cluster, user)
	if err != nil {
		return ""
	}

	// election.leader is cluster-wide, so the first reachable instance answers.
	leader := ""
	forEachInstance(ctx, cluster, rs, settings.Port, rs.GetReplicas(), observeBudget, func(ctx context.Context, addr string) bool {
		name, err := readElectedLeader(ctx, addr, user, password)
		if err != nil {
			return false // unreachable; try the next instance
		}
		leader = name
		return true
	})
	return leader
}

// reloadEvalScript triggers config:reload() and returns the instance's loaded
// config version (the operator_config_hash label). If the instance already runs the
// desired version it skips the reload. %s is the desired hash.
//
// When the reload fails — or succeeds but leaves the config in a non-ready state
// (Tarantool raises alerts, e.g. a static option that needs a restart) — the
// return value is "<loaded>\t<reason>", so the operator surfaces Tarantool's own
// explanation instead of requeueing forever in silence.
const reloadEvalScript = `
local config = require('config')
local function loaded()
    local l = config:get('labels')
    return (l and l.operator_config_hash) or ""
end
if loaded() == %q then return loaded() end
local ok, err = pcall(function() config:reload() end)
if not ok then return loaded() .. '\t' .. tostring(err) end
local info = config:info()
if info.status ~= 'ready' and info.alerts ~= nil and next(info.alerts) ~= nil then
    local msgs = {}
    for _, a in pairs(info.alerts) do msgs[#msgs + 1] = tostring(a.message) end
    return loaded() .. '\t' .. table.concat(msgs, '; ')
end
return loaded()
`

// reloadConfig drives the operator-driven config reload: it connects to the
// first `instances` instances over iproto (as the super user) and calls
// config:reload() so dynamic config changes apply without a pod restart, then
// reads back which config version each instance is running. It returns whether
// ALL addressed instances confirmed the desired version (the scale-up gate
// keys off this) and whether any instance was reachable but still on an older
// version (the mounted config file may not have synced yet — worth a requeue).
// Best-effort: unreachable instances are skipped (the next reconcile retries)
// and no super user means no reload (callers gate the dynamic-key rollout
// exclusion on a super user).
func (r *ReplicaSetReconciler) reloadConfig(ctx context.Context, cluster *v2alpha1.Cluster, rs *v2alpha1.ReplicaSet, settings clusterconfig.Settings, desiredHash string, instances int32) (allConverged, retry bool, applyErr string) {
	user := settings.SuperUser
	if user == "" || desiredHash == "" {
		return false, false, ""
	}
	password, err := resolveUserPassword(ctx, r.Client, cluster, user)
	if err != nil {
		return false, false, ""
	}

	converged, reachable := int32(0), int32(0)
	applyFailure := ""
	forEachInstance(ctx, cluster, rs, settings.Port, instances, observeBudget, func(ctx context.Context, addr string) bool {
		loaded, instErr, err := reloadInstanceConfig(ctx, addr, user, password, desiredHash)
		if err != nil {
			return false // unreachable; best-effort, retry next reconcile
		}
		reachable++
		if loaded == desiredHash {
			converged++
		}
		// Keep the first per-instance apply failure: with the config Secret shared
		// by the whole set, every instance fails the same way — one reason suffices.
		// The caller surfaces it (Degraded condition + ConfigRejected event).
		if instErr != "" && applyFailure == "" {
			applyFailure = fmt.Sprintf("%s: %s", addr, instErr)
		}
		return false
	})
	return converged == instances, reachable > 0 && converged < instances, applyFailure
}

// needsNetworkLeaderObservation reports whether the leader must be read from a
// running instance over iproto (election/supervised) rather than being declared
// by the operator (manual/off).
func needsNetworkLeaderObservation(failover string) bool {
	switch failover {
	case clusterconfig.FailoverManual, clusterconfig.FailoverOff:
		return false
	case clusterconfig.FailoverElection, clusterconfig.FailoverSupervised:
		return true
	}
	return true // unknown/future modes: assume the leader must be observed
}

// resolveUserPassword reads userName's password (data key = user name), always
// in the Cluster's namespace: from the credentialsSecret, falling back — for
// the built-in operator user — to the generated <cluster>-operator-user Secret.
// The order mirrors the render-side underlay: a user-managed password for the
// operator user wins over the generated one.
func resolveUserPassword(ctx context.Context, c client.Client, cluster *v2alpha1.Cluster, userName string) (string, error) {
	if name := cluster.Spec.CredentialsSecret; name != "" {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: name}, secret); err != nil {
			return "", err
		}
		if value, ok := secret.Data[userName]; ok {
			return string(value), nil
		}
	}
	if userName == clusterconfig.OperatorUser {
		secret := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: OperatorUserSecretName(cluster.Name)}, secret); err == nil {
			if value, ok := secret.Data[userName]; ok {
				return string(value), nil
			}
		}
	}
	return "", fmt.Errorf("no password for user %q in cluster %s/%s credentials", userName, cluster.Namespace, cluster.Name)
}

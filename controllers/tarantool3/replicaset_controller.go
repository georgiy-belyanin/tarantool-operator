package tarantool3

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

//+kubebuilder:rbac:groups=db.tarantool.io,resources=replicasets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=db.tarantool.io,resources=replicasets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=db.tarantool.io,resources=replicasets/finalizers,verbs=update
//+kubebuilder:rbac:groups=db.tarantool.io,resources=clusters,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// configReloadRequeue is how soon to retry the operator-driven config reload when
// an instance is reachable but its mounted config file hasn't synced to the new
// version yet (kubelet projected-volume update lag).
const configReloadRequeue = 10 * time.Second

// ReplicaSetReconciler renders one ReplicaSet into a StatefulSet whose pods are
// the Tarantool 3 instances, mounting the cluster's rendered config Secret.
type ReplicaSetReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// NewReplicaSetReconciler builds a ReplicaSetReconciler from the manager.
func NewReplicaSetReconciler(mgr ctrl.Manager) *ReplicaSetReconciler {
	return &ReplicaSetReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("tarantool3-replicaset"),
	}
}

func (r *ReplicaSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	rs := &v2alpha1.ReplicaSet{}
	if err := r.Get(ctx, req.NamespacedName, rs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !rs.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, rs)
	}

	cluster := &v2alpha1.Cluster{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: rs.Spec.ClusterName}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("waiting for cluster", "cluster", rs.Spec.ClusterName)
			return ctrl.Result{}, r.applyStatus(ctx, rs, pendingObserved(rs))
		}
		return ctrl.Result{}, err
	}

	// The Cluster reconciler renders the config Secret; wait for it and use its
	// hash to stamp the pods so they roll when the config changes.
	secret := &corev1.Secret{}
	secretName := ConfigSecretName(cluster.Name)
	if err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: secretName}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("waiting for config secret", "secret", secretName)
			return ctrl.Result{}, r.applyStatus(ctx, rs, pendingObserved(rs))
		}
		return ctrl.Result{}, err
	}
	// The Cluster reconciler can briefly render the config before this ReplicaSet is
	// observed in its cache (informer warmup), producing a topology-less config. Do
	// not build the StatefulSet from such a config: its config-hash would change once
	// our subtree appears and roll the pods — and on ephemeral storage that rolls the
	// designated bootstrap leader (<rs>-0) into re-bootstrapping its own split replica
	// set. Wait for the config Secret to contain our subtree; the Cluster re-renders
	// (and our Secret watch re-triggers us) once it observes this ReplicaSet.
	delivered, err := clusterconfig.ParseDelivered(secret.Data[ConfigFileName])
	if err != nil {
		return ctrl.Result{}, err
	}
	if !delivered.ContainsReplicaSet(rs.GetGroup(), rs.Name) {
		logger.Info("waiting for config to include this replicaset", "secret", secretName)
		return ctrl.Result{RequeueAfter: missingSecretRequeue}, r.applyStatus(ctx, rs, pendingObserved(rs))
	}

	// The operator's own gates (failover mode, manual leader, iproto port) read
	// the DELIVERED config — the single merged truth — so they behave the same
	// whether a value came from a typed spec field or a raw config section.
	settings := delivered.Settings(rs.GetGroup(), rs.Name)

	// A vshard storage holds buckets, so deleting it must drain them first or data
	// is lost. Gate deletion behind a finalizer — but only when the operator can
	// actually verify the drain over iproto (a "super" user), matching the
	// degradation contract of the other iproto-dependent features. Without one,
	// no finalizer is added and deletion is the plain (unsafe) StatefulSet GC.
	if delivered.IsShardStorage(rs.GetGroup(), rs.Name) && settings.SuperUser != "" {
		if !controllerutil.ContainsFinalizer(rs, ShardDrainFinalizer) {
			patch := client.MergeFrom(rs.DeepCopy())
			controllerutil.AddFinalizer(rs, ShardDrainFinalizer)
			if err := r.Patch(ctx, rs, patch); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// Roll this replica set's pods only when ITS OWN effective config (the global
	// scope + its own subtree) changes — not when an unrelated replica set changes.
	// Computed from the delivered config in the Secret.
	//
	// When a "super" credential user is configured, the operator can hot-reload
	// dynamic config changes over iproto (config:reload), so use RolloutHash, which
	// excludes the dynamic keys — a change to only those does not roll the pods; the
	// operator reloads them below. Without a super user the operator can't reload, so
	// fall back to InstanceScopeHash (every change rolls — correct, just not hot).
	//
	// deliveredHash names the config version instances should LOAD (the Secret's
	// full-content hash, verified by reloadConfig); rolloutHash is what ROLLS the
	// pods (stamped on the pod template).
	deliveredHash := secret.Annotations[ConfigHashAnnotation]
	scopeHashFn := delivered.InstanceScopeHash
	if settings.SuperUser != "" {
		scopeHashFn = delivered.RolloutHash
	}
	rolloutHash, err := scopeHashFn(rs.GetGroup(), rs.Name)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Scale-up ordering: a new instance may only be created once the delivered
	// config already lists it AND every existing instance has loaded that config.
	// Otherwise the new pod joins a master whose loaded config does not contain
	// it yet — the master's autoexpel expels the newcomer on the spot (poisoning
	// its fresh UUID), and the master's subsequent reload can deadlock read-only
	// on the expelled peers (verified live on kind). Held back, the StatefulSet
	// stays at its current size and we retry shortly.
	replicas, held, err := r.scaleUpReplicas(ctx, cluster, rs, settings, delivered, deliveredHash)
	if err != nil {
		return ctrl.Result{}, err
	}
	if held {
		r.Recorder.Event(rs, corev1.EventTypeNormal, "ScaleUpPending",
			"waiting for the existing instances to load the expanded topology before creating new ones")
	}

	current, ready, err := r.reconcileStatefulSet(ctx, cluster, rs, settings, secret.Name, rolloutHash, replicas)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcilePodDisruptionBudget(ctx, cluster, rs); err != nil {
		return ctrl.Result{}, err
	}

	// Phase: Configuring while the set converges for the first time; Ready when
	// every instance is ready; Degraded when a previously fully-ready set (the
	// sticky Bootstrapped flag) has lost instances — a regression, not a
	// configuration in progress. (The Degraded CONDITION stays quorum-based;
	// the phase reports any post-bootstrap regression.)
	o := observed{phase: v2alpha1.ReplicaSetConfiguring, current: current, ready: ready}
	switch {
	case ready == rs.GetReplicas():
		o.phase = v2alpha1.ReplicaSetReady
		// Sticky: once the whole set has been ready, mark it bootstrapped. This
		// gates replication.autoexpel (enabled only after the set has formed) so it
		// can't race the initial bootstrap. Never reset, so it doesn't flap on later
		// restarts. The Cluster reconciler watches ReplicaSet changes and re-renders
		// the config to add autoexpel when this flips.
		rs.Status.Bootstrapped = true
	case rs.Status.Bootstrapped:
		o.phase = v2alpha1.ReplicaSetDegraded
	}

	isRouter := delivered.IsShardRouter(rs.GetGroup(), rs.Name)
	requeueAfter, leader, configError := r.runtimeOps(ctx, cluster, rs, settings, deliveredHash, ready, replicas, isRouter)
	o.leader = leader
	o.configError = configError
	// A config Tarantool rejected (typo, bad value) leaves the set unable to
	// converge; report Error rather than a misleading Configuring/Degraded so the
	// failure names itself. Cleared automatically when the reason goes away.
	if configError != "" && ready < rs.GetReplicas() {
		o.phase = v2alpha1.ReplicaSetError
	}
	// Re-check while the set has not converged to its desired size. Pods may
	// still be coming up, the scale-up gate (held) may be pausing, or an
	// instance may be crash-looping — and a startup config rejection is only
	// readable from pod status AFTER the crash. The RS controller owns the
	// StatefulSet but not its Pods, so a post-startup crash emits no event;
	// this requeue is what lets detectConfigRejection (and general convergence)
	// be re-evaluated. A converged set requeues nothing, so there is no loop.
	if requeueAfter == 0 && ready < rs.GetReplicas() {
		requeueAfter = configReloadRequeue
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, r.applyStatus(ctx, rs, o)
}

// scaleUpReplicas decides how many StatefulSet replicas to request this pass.
// Scaling DOWN (or steady state) always proceeds; scaling UP is held at the
// current size until the delivered config lists every desired instance and all
// existing instances confirm they run it. Without a super user there is
// nothing to order — the operator cannot reload, so a topology change rolls
// every pod and the restart path tolerates the window.
func (r *ReplicaSetReconciler) scaleUpReplicas(ctx context.Context, cluster *v2alpha1.Cluster, rs *v2alpha1.ReplicaSet, settings clusterconfig.Settings, delivered *clusterconfig.Delivered, deliveredHash string) (replicas int32, held bool, err error) {
	desired := rs.GetReplicas()
	if settings.SuperUser == "" {
		return desired, false, nil
	}

	existing := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: rs.Name}, existing); err != nil {
		if apierrors.IsNotFound(err) {
			return desired, false, nil // initial creation: the set bootstraps jointly
		}
		return 0, false, err
	}
	if existing.Spec.Replicas == nil || *existing.Spec.Replicas >= desired {
		return desired, false, nil // not a scale-up
	}
	current := *existing.Spec.Replicas

	// The rendered topology must already include the new instances (the Cluster
	// reconciler may not have re-rendered yet)...
	if delivered.InstanceCount(rs.GetGroup(), rs.Name) < int(desired) {
		return current, true, nil
	}
	// ...and every EXISTING instance must have loaded it, so the masters know
	// the newcomers before the newcomers knock.
	converged, _, _ := r.reloadConfig(ctx, cluster, rs, settings, deliveredHash, current)
	if !converged {
		return current, true, nil
	}
	return desired, false, nil
}

// runtimeOps performs the operations that talk to the RUNNING cluster — dead-node
// remediation, leader observation, dynamic-config reload and the post-rollout
// schema upgrade — all best-effort with the same inputs. Returns how soon to
// requeue (0 = no requeue), the observed leader, and a config-rejection reason
// (from a dynamic reload or a CrashLooping startup) when Tarantool refused the
// delivered config — "" otherwise.
func (r *ReplicaSetReconciler) runtimeOps(ctx context.Context, cluster *v2alpha1.Cluster, rs *v2alpha1.ReplicaSet, settings clusterconfig.Settings, deliveredHash string, ready, instances int32, isRouter bool) (requeueAfter time.Duration, leader, configError string) {
	// Dead-node remediation: force-delete instance pods stranded on unreachable
	// nodes (or stuck Terminating) so the StatefulSet can reschedule them.
	if r.remediateStuckPods(ctx, settings, rs, ready) {
		requeueAfter = remediationRequeue // candidate inside the grace window
	}

	// A startup config rejection (Tarantool exits before iproto is up) is read
	// from the CrashLooping pod's status, so it works even when nothing is ready.
	if reason, instance := r.detectConfigRejection(ctx, rs); reason != "" {
		configError = fmt.Sprintf("%s: %s", instance, reason)
	}
	if ready == 0 {
		if configError != "" {
			r.recordConfigRejected(rs, configError)
		}
		return requeueAfter, "", configError // nothing is up; nothing to talk to
	}

	leader = r.observeLeader(ctx, cluster, rs, settings)
	// In election/supervised modes the leader is read over iproto, which needs
	// a "super" credential user. Without one, status.leader stays empty — surface
	// that rather than degrading silently.
	if leader == "" && needsNetworkLeaderObservation(settings.Failover) {
		leaderObservationFailuresTotal.Inc()
		if settings.SuperUser == "" {
			r.Recorder.Event(rs, corev1.EventTypeWarning, "LeaderObservationDisabled",
				"status.leader is unavailable: observing the elected leader requires a credentials user with the \"super\" role")
		}
	}
	// Apply dynamic config changes without a pod restart by reloading each
	// instance (config:reload over iproto). If an instance is reachable but still
	// on an older config version (its mounted config file hasn't synced yet),
	// requeue to retry shortly.
	if settings.SuperUser != "" {
		_, retry, applyErr := r.reloadConfig(ctx, cluster, rs, settings, deliveredHash, instances)
		if retry {
			reloadRetryTotal.Inc()
			requeueAfter = configReloadRequeue
		}
		// A live reload rejection is the strongest signal (the instance's own
		// verdict on a running box); prefer it over a startup crash reason.
		if applyErr != "" {
			configError = applyErr
		}
	}
	// Leader-aware rollout: with the StatefulSet on OnDelete, advance the roll
	// by one pod per pass — followers first, the observed leader last.
	if operatorManagedRollout(rs, settings) {
		if r.orchestrateRollout(ctx, rs, leader) {
			requeueAfter = rolloutRequeue
		}
	}
	// After an image rollout completes, upgrade the system schema on the
	// leader (gated on homogeneous binary versions; persisted via applyStatus).
	_ = r.maybeUpgradeSchema(ctx, cluster, rs, settings, ready)
	// For a vshard router, bootstrap vshard once the storages are reachable so a
	// fresh sharded cluster distributes its buckets without a manual step. Retry
	// (requeue) until the storages answer.
	if isRouter && r.maybeBootstrapVshard(ctx, cluster, rs, settings, ready) {
		requeueAfter = shardBootstrapRequeue
	}
	if configError != "" {
		r.recordConfigRejected(rs, configError)
	}
	return requeueAfter, leader, configError
}

// recordConfigRejected emits a ConfigRejected warning, deduped: only when the
// rejection is new or its reason changed since the last reconcile, so a steady
// CrashLoop or repeated reload failure does not spam events. Dedup keys off the
// Degraded condition the previous applyStatus wrote.
func (r *ReplicaSetReconciler) recordConfigRejected(rs *v2alpha1.ReplicaSet, reason string) {
	prev := meta.FindStatusCondition(rs.Status.Conditions, ConditionDegraded)
	if prev != nil && prev.Reason == "ConfigRejected" && prev.Message == reason {
		return // already reported this exact verdict
	}
	r.Recorder.Eventf(rs, corev1.EventTypeWarning, "ConfigRejected",
		"Tarantool rejected the delivered configuration: %s", reason)
}

// reconcileStatefulSet creates or updates the StatefulSet and returns the current
// (observed) and ready instance counts.
func (r *ReplicaSetReconciler) reconcileStatefulSet(ctx context.Context, cluster *v2alpha1.Cluster, rs *v2alpha1.ReplicaSet, settings clusterconfig.Settings, secretName, configHash string, replicas int32) (current, ready int32, err error) {
	desired := DesiredStatefulSet(cluster, rs, secretName, configHash, replicas)
	injectLeaderStepDown(&desired.Spec.Template, settings.Failover)
	// Operator-managed (leader-aware) rollouts: the StatefulSet must not roll
	// pods on its own — the operator deletes them in its own order (followers
	// first, leader last; see rollout.go). Only when the user left the
	// strategy unset; an explicit updateStrategy is passed through verbatim.
	if operatorManagedRollout(rs, settings) {
		desired.Spec.UpdateStrategy = appsv1.StatefulSetUpdateStrategy{Type: appsv1.OnDeleteStatefulSetStrategyType}
	}

	sts := &appsv1.StatefulSet{}
	sts.Name = desired.Name
	sts.Namespace = desired.Namespace

	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, sts, func() error {
		sts.Labels = desired.Labels
		// Selector, ServiceName, VolumeClaimTemplates and PodManagementPolicy are
		// immutable after creation; we set them unconditionally and they only take
		// effect on create (an unchanged value is a no-op on update).
		sts.Spec.Selector = desired.Spec.Selector
		sts.Spec.ServiceName = desired.Spec.ServiceName
		sts.Spec.PodManagementPolicy = desired.Spec.PodManagementPolicy
		sts.Spec.VolumeClaimTemplates = desired.Spec.VolumeClaimTemplates
		sts.Spec.Replicas = desired.Spec.Replicas
		sts.Spec.RevisionHistoryLimit = desired.Spec.RevisionHistoryLimit
		sts.Spec.MinReadySeconds = desired.Spec.MinReadySeconds
		sts.Spec.UpdateStrategy = desired.Spec.UpdateStrategy
		sts.Spec.PersistentVolumeClaimRetentionPolicy = desired.Spec.PersistentVolumeClaimRetentionPolicy
		sts.Spec.Template = desired.Spec.Template
		return controllerutil.SetControllerReference(rs, sts, r.Scheme)
	}); err != nil {
		return 0, 0, fmt.Errorf("reconciling statefulset: %w", err)
	}

	// Re-read the (possibly newly created) StatefulSet status for the counts.
	if err := r.Get(ctx, types.NamespacedName{Namespace: sts.Namespace, Name: sts.Name}, sts); err != nil {
		return 0, 0, err
	}
	return sts.Status.Replicas, sts.Status.ReadyReplicas, nil
}

// reconcilePodDisruptionBudget creates/updates a PDB (maxUnavailable=1) for a
// multi-instance replica set so node drains can't take down quorum, and removes it
// for a single-instance set (a PDB there would only block draining the lone node).
func (r *ReplicaSetReconciler) reconcilePodDisruptionBudget(ctx context.Context, cluster *v2alpha1.Cluster, rs *v2alpha1.ReplicaSet) error {
	name := types.NamespacedName{Namespace: rs.Namespace, Name: PDBName(rs.Name)}

	if rs.GetReplicas() < 2 {
		existing := &policyv1.PodDisruptionBudget{}
		if err := r.Get(ctx, name, existing); err != nil {
			return client.IgnoreNotFound(err)
		}
		return client.IgnoreNotFound(r.Delete(ctx, existing))
	}

	desired := DesiredPodDisruptionBudget(cluster, rs)
	pdb := &policyv1.PodDisruptionBudget{}
	pdb.Name = desired.Name
	pdb.Namespace = desired.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pdb, func() error {
		pdb.Labels = desired.Labels
		// Selector is immutable after creation; set unconditionally (no-op on update).
		pdb.Spec.Selector = desired.Spec.Selector
		pdb.Spec.MaxUnavailable = desired.Spec.MaxUnavailable
		return controllerutil.SetControllerReference(rs, pdb, r.Scheme)
	})
	return err
}

// observed is what one reconcile pass concluded about the replica set; it is
// the input to applyStatus, the single owner of all status writes. (The sticky
// transitions — Status.Bootstrapped, Status.SchemaUpgradedForImage — are set on
// rs.Status where they are discovered and persisted by the same update.)
type observed struct {
	phase   v2alpha1.ReplicaSetPhase
	current int32 // StatefulSet's observed replicas
	ready   int32
	leader  string
	// configError is Tarantool's reason for rejecting the delivered config
	// (startup CrashLoop or dynamic reload); "" when the config was accepted.
	// When set, applyStatus reports it on the Degraded condition.
	configError string
}

// pendingObserved is the early-exit shape: Pending, nothing ready, keep the
// previously observed replica count.
func pendingObserved(rs *v2alpha1.ReplicaSet) observed {
	return observed{phase: v2alpha1.ReplicaSetPending, current: rs.Status.Replicas}
}

// applyStatus writes the whole ReplicaSet status from o in one place.
func (r *ReplicaSetReconciler) applyStatus(ctx context.Context, rs *v2alpha1.ReplicaSet, o observed) error {
	rs.Status.Phase = o.phase
	rs.Status.Replicas = o.current
	rs.Status.ObservedGeneration = rs.Generation
	rs.Status.Leader = o.leader
	// status.selector backs the scale subresource (kubectl scale / HPA): the
	// serialized label selector for this replica set's instance pods.
	rs.Status.Selector = labels.SelectorFromSet(replicaSetLabels(rs.Spec.ClusterName, rs.GetGroup(), rs.Name)).String()
	rs.SetReadyInstances(o.ready)
	meta.SetStatusCondition(&rs.Status.Conditions,
		condition(ConditionReady, o.phase == v2alpha1.ReplicaSetReady, string(o.phase), rs.Generation))
	for _, c := range replicaSetConditions(rs, o.ready) {
		meta.SetStatusCondition(&rs.Status.Conditions, c)
	}
	// A config rejection overrides the (quorum-based) Degraded condition with the
	// specific reason — the root cause. When o.configError is "", the quorum
	// condition above stands, so a fixed config clears this with no extra step.
	if o.configError != "" {
		c := condition(ConditionDegraded, true, "ConfigRejected", rs.Generation)
		c.Message = o.configError
		meta.SetStatusCondition(&rs.Status.Conditions, c)
	}
	return r.Status().Update(ctx, rs)
}

// SetupWithManager registers the controller. It owns its StatefulSet and watches
// the config Secret so a config change re-stamps and rolls the pods.
func (r *ReplicaSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v2alpha1.ReplicaSet{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		// Only the operator's rendered config Secrets matter here; without the
		// predicate every Secret write anywhere in the namespace would run the
		// map function (a ReplicaSet list) for nothing.
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapConfigSecretToReplicaSets),
			builder.WithPredicates(predicate.NewPredicateFuncs(isOperatorConfigSecret))).
		Complete(r)
}

// isOperatorConfigSecret matches the operator's rendered config Secrets.
func isOperatorConfigSecret(obj client.Object) bool {
	labels := obj.GetLabels()
	return labels[ManagedByLabel] == ManagedByValue && labels[ClusterLabel] != ""
}

// mapConfigSecretToReplicaSets enqueues every ReplicaSet of the cluster whose
// rendered config Secret changed.
func (r *ReplicaSetReconciler) mapConfigSecretToReplicaSets(ctx context.Context, obj client.Object) []reconcile.Request {
	clusterName, ok := obj.GetLabels()[ClusterLabel]
	if !ok {
		return nil
	}

	list := &v2alpha1.ReplicaSetList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}

	var requests []reconcile.Request
	for i := range list.Items {
		if list.Items[i].Spec.ClusterName == clusterName {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{Namespace: list.Items[i].Namespace, Name: list.Items[i].Name},
			})
		}
	}
	return requests
}

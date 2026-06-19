package tarantool3

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
	"github.com/tarantool/tarantool-operator/pkg/clusterconfig"
)

// missingSecretRequeue is how soon to retry when a referenced credential Secret
// is not yet present (belt-and-suspenders alongside the Secret watch).
const missingSecretRequeue = 10 * time.Second

//+kubebuilder:rbac:groups=db.tarantool.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=db.tarantool.io,resources=clusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=db.tarantool.io,resources=clusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=db.tarantool.io,resources=replicasets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// ClusterReconciler renders a Cluster's declarative configuration into a Secret
// and owns the headless Service that gives instances stable DNS names.
type ClusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// NewClusterReconciler builds a ClusterReconciler from the manager.
func NewClusterReconciler(mgr ctrl.Manager) *ClusterReconciler {
	return &ClusterReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("tarantool3-cluster"),
	}
}

func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cluster := &v2alpha1.Cluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !cluster.DeletionTimestamp.IsZero() {
		// Owned Service and Secret are garbage-collected via owner references.
		return ctrl.Result{}, nil
	}

	replicaSets, err := r.replicaSetsForCluster(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	passwords, err := r.resolvePasswords(ctx, cluster)
	if err != nil {
		// A referenced credential Secret is missing/incomplete. This is transient
		// (the Secret may be created or rotated shortly), so report Pending and
		// requeue; the Secret watch also re-triggers us when it appears.
		logger.Info("waiting for credential secrets", "reason", err.Error())
		if e := r.setStatus(ctx, cluster, v2alpha1.ClusterPending, replicaSets); e != nil {
			return ctrl.Result{}, e
		}
		return ctrl.Result{RequeueAfter: missingSecretRequeue}, nil
	}

	input := clusterconfig.Input{Cluster: cluster, ReplicaSets: replicaSets, Passwords: passwords}

	// Build the config tree once for both the rendered YAML and its change-detection
	// hash (instead of Render + Hash, which would build it twice).
	rendered, hash, err := clusterconfig.RenderAndHash(input)
	if err != nil {
		renderTotal.WithLabelValues("error").Inc()
		logger.Error(err, "rendering cluster config")
		return ctrl.Result{}, r.setStatus(ctx, cluster, v2alpha1.ClusterError, replicaSets)
	}
	renderTotal.WithLabelValues("ok").Inc()

	if err := r.reconcileService(ctx, cluster); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.reconcileConfigSecret(ctx, cluster, rendered, hash); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, r.setStatus(ctx, cluster, v2alpha1.ClusterReady, replicaSets)
}

func (r *ClusterReconciler) replicaSetsForCluster(ctx context.Context, cluster *v2alpha1.Cluster) ([]v2alpha1.ReplicaSet, error) {
	list := &v2alpha1.ReplicaSetList{}
	if err := r.List(ctx, list, client.InNamespace(cluster.Namespace)); err != nil {
		return nil, err
	}

	owned := make([]v2alpha1.ReplicaSet, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.ClusterName == cluster.Name {
			owned = append(owned, list.Items[i])
		}
	}
	return owned, nil
}

// resolvePasswords reads the cluster's credentialsSecret (same namespace): each
// data key is a Tarantool user name, the value its password. The renderer
// injects them into matching config-declared users, so passwords never appear
// in the CR.
func (r *ClusterReconciler) resolvePasswords(ctx context.Context, cluster *v2alpha1.Cluster) (map[string]string, error) {
	passwords := map[string]string{}

	// Base: the in-namespace credentialsSecret (data key = user name).
	if name := cluster.Spec.CredentialsSecret; name != "" {
		secret := &corev1.Secret{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: cluster.Namespace, Name: name}, secret); err != nil {
			return nil, fmt.Errorf("credentials secret %s/%s: %w", cluster.Namespace, name, err)
		}
		for user, pw := range secret.Data {
			passwords[user] = string(pw)
		}
	}

	// Optional external sources (e.g. Vault, behind the `vault` build tag) overlay
	// the base, so a cluster can keep some passwords in the Secret and source
	// others externally. Absent any compiled-in source this loop is a no-op.
	for _, src := range passwordSources {
		extra, err := src.Passwords(ctx, r.Client, cluster)
		if err != nil {
			return nil, fmt.Errorf("password source %q: %w", src.Name(), err)
		}
		for user, pw := range extra {
			passwords[user] = pw
		}
	}

	// Built-in operator user: generated password as an UNDERLAY — if the user
	// manages a password for this user themselves (credentialsSecret or an
	// external source), theirs wins and the generated Secret is left unused.
	operatorPassword, err := r.ensureOperatorUserSecret(ctx, cluster)
	if err != nil {
		return nil, err
	}
	if _, has := passwords[clusterconfig.OperatorUser]; !has && operatorPassword != "" {
		passwords[clusterconfig.OperatorUser] = operatorPassword
	}
	return passwords, nil
}

// ensureOperatorUserSecret guarantees the <cluster>-operator-user Secret exists
// with a generated password for the built-in operator user (and returns it), or
// removes the Secret when the feature is explicitly disabled. The password is
// generated once and kept stable across reconciles.
func (r *ClusterReconciler) ensureOperatorUserSecret(ctx context.Context, cluster *v2alpha1.Cluster) (string, error) {
	name := types.NamespacedName{Namespace: cluster.Namespace, Name: OperatorUserSecretName(cluster.Name)}
	secret := &corev1.Secret{}
	err := r.Get(ctx, name, secret)

	if cluster.Spec.DisableOperatorUser {
		if err == nil {
			return "", r.Delete(ctx, secret)
		}
		return "", client.IgnoreNotFound(err)
	}

	if err == nil {
		return string(secret.Data[clusterconfig.OperatorUser]), nil
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}

	password, err := generatePassword()
	if err != nil {
		return "", err
	}
	secret = &corev1.Secret{}
	secret.Name = name.Name
	secret.Namespace = name.Namespace
	secret.Labels = clusterLabels(cluster.Name)
	secret.Type = corev1.SecretTypeOpaque
	secret.Data = map[string][]byte{clusterconfig.OperatorUser: []byte(password)}
	if err := controllerutil.SetControllerReference(cluster, secret, r.Scheme); err != nil {
		return "", err
	}
	if err := r.Create(ctx, secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Lost a create race (concurrent reconcile); read the winner's password.
			if err := r.Get(ctx, name, secret); err != nil {
				return "", err
			}
			return string(secret.Data[clusterconfig.OperatorUser]), nil
		}
		return "", err
	}
	return password, nil
}

// generatePassword returns a cryptographically random password (192 bits,
// URL-safe base64).
func generatePassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (r *ClusterReconciler) reconcileService(ctx context.Context, cluster *v2alpha1.Cluster) error {
	desired := DesiredHeadlessService(cluster)
	svc := &corev1.Service{}
	svc.Name = desired.Name
	svc.Namespace = desired.Namespace

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = desired.Labels
		svc.Spec.ClusterIP = corev1.ClusterIPNone
		svc.Spec.PublishNotReadyAddresses = true
		svc.Spec.Selector = desired.Spec.Selector
		svc.Spec.Ports = desired.Spec.Ports
		return controllerutil.SetControllerReference(cluster, svc, r.Scheme)
	})
	return err
}

func (r *ClusterReconciler) reconcileConfigSecret(ctx context.Context, cluster *v2alpha1.Cluster, rendered []byte, hash string) error {
	secret := &corev1.Secret{}
	secret.Name = ConfigSecretName(cluster.Name)
	secret.Namespace = cluster.Namespace

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = clusterLabels(cluster.Name)
		secret.Type = corev1.SecretTypeOpaque
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		// Only (re)write the rendered config when its content hash actually changed.
		// The render is deterministic (sigs.k8s.io/yaml emits sorted keys), so an
		// unchanged config already serializes to identical bytes; gating on the hash
		// additionally avoids re-parsing the payload for the grow-only memtx check
		// below on every steady-state reconcile, and keeps the no-op explicit.
		if secret.Annotations[ConfigHashAnnotation] != hash || secret.Data[ConfigFileName] == nil {
			// memtx.memory is grow-only at runtime: a decrease can be neither
			// hot-reloaded nor applied by box.cfg, so it would go silently stale.
			// Warn so the user knows a manual instance restart is required.
			if secret.Data[ConfigFileName] != nil {
				memtxOf := func(payload []byte) (int64, bool) {
					d, err := clusterconfig.ParseDelivered(payload)
					if err != nil {
						return 0, false
					}
					return d.MemtxMemory()
				}
				oldMem, oldOK := memtxOf(secret.Data[ConfigFileName])
				newMem, newOK := memtxOf(rendered)
				if oldOK && newOK && newMem < oldMem {
					r.Recorder.Eventf(cluster, corev1.EventTypeWarning, "MemtxShrinkRequiresRestart",
						"memtx.memory decreased from %d to %d, which cannot be applied at runtime (grow-only); restart the instance pods to apply it",
						oldMem, newMem)
				}
			}
			secret.Annotations[ConfigHashAnnotation] = hash
			secret.Data = map[string][]byte{ConfigFileName: rendered}
		}
		return controllerutil.SetControllerReference(cluster, secret, r.Scheme)
	})
	return err
}

func (r *ClusterReconciler) setStatus(ctx context.Context, cluster *v2alpha1.Cluster, phase v2alpha1.ClusterPhase, replicaSets []v2alpha1.ReplicaSet) error {
	cluster.Status.Phase = phase
	cluster.Status.ObservedGeneration = cluster.Generation
	meta.SetStatusCondition(&cluster.Status.Conditions,
		condition(ConditionReady, phase == v2alpha1.ClusterReady, string(phase), cluster.Generation))
	// Deployment-style triad (Available duplicates Ready for a Cluster — its job is
	// render+deliver — so only Progressing/Degraded are added here).
	progressing := phase == v2alpha1.ClusterPending || phase == v2alpha1.ClusterConfiguring
	meta.SetStatusCondition(&cluster.Status.Conditions,
		condition(ConditionProgressing, progressing, string(phase), cluster.Generation))
	meta.SetStatusCondition(&cluster.Status.Conditions,
		condition(ConditionDegraded, phase == v2alpha1.ClusterError, string(phase), cluster.Generation))
	// The data-plane view: Ready above only covers render+delivery, so aggregate
	// the children's Available here — a cluster whose instances are down must not
	// present as fully healthy. (The ReplicaSet watch re-triggers this reconcile
	// whenever a child's status changes, keeping the aggregate fresh.)
	meta.SetStatusCondition(&cluster.Status.Conditions,
		membersAvailableCondition(replicaSets, cluster.Generation))
	// Leadership is per-replicaset in Tarantool 3; see ReplicaSet.status.leader.
	return r.Status().Update(ctx, cluster)
}

// credentialsSecretField indexes Clusters by spec.credentialsSecret, so a Secret
// event resolves to the referencing Clusters via an indexed lookup instead of
// scanning every Cluster in the namespace on every Secret write.
const credentialsSecretField = ".spec.credentialsSecret"

// indexClusterByCredentialsSecret is the credentialsSecretField index function.
func indexClusterByCredentialsSecret(obj client.Object) []string {
	cluster, ok := obj.(*v2alpha1.Cluster)
	if !ok || cluster.Spec.CredentialsSecret == "" {
		return nil
	}
	return []string{cluster.Spec.CredentialsSecret}
}

// SetupWithManager registers the controller. It reconciles a Cluster when any of
// its ReplicaSets change (so adding a replicaset re-renders the config), owns the
// Service and config Secret it produces, and watches credential Secrets so a
// newly created or rotated password is picked up promptly.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v2alpha1.Cluster{},
		credentialsSecretField, indexClusterByCredentialsSecret); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v2alpha1.Cluster{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Watches(&v2alpha1.ReplicaSet{}, handler.EnqueueRequestsFromMapFunc(mapReplicaSetToCluster)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapSecretToClusters)).
		Complete(r)
}

func mapReplicaSetToCluster(_ context.Context, obj client.Object) []reconcile.Request {
	rs, ok := obj.(*v2alpha1.ReplicaSet)
	if !ok || rs.Spec.ClusterName == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Namespace: rs.Namespace, Name: rs.Spec.ClusterName},
	}}
}

// mapSecretToClusters enqueues every Cluster in the Secret's namespace whose
// credentialsSecret references it (an indexed lookup, see credentialsSecretField),
// so credential creation/rotation triggers a re-render. (Credential Secrets are
// read only from the Cluster's namespace.)
func (r *ClusterReconciler) mapSecretToClusters(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &v2alpha1.ClusterList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{credentialsSecretField: obj.GetName()}); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: list.Items[i].Namespace, Name: list.Items[i].Name},
		})
	}
	return requests
}

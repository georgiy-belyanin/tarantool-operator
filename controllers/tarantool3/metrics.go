package tarantool3

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Operator metrics for the Tarantool 3 controllers, registered on the
// controller-runtime registry (served on the manager's metrics endpoint alongside
// the built-in controller_runtime_* metrics).
var (
	// renderTotal counts cluster config renders by result. A rising error rate
	// means clusters are stuck in Error (invalid config / schema rejection).
	renderTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "tarantool3_config_render_total",
		Help: "Cluster configuration renders by the Tarantool 3 operator, by result (ok|error).",
	}, []string{"result"})

	// reloadRetryTotal counts reconciles where an instance was reachable but still
	// on an older config version after a reload attempt (mounted Secret not synced
	// yet). A persistently rising rate means hot-reload is not converging.
	reloadRetryTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tarantool3_config_reload_retries_total",
		Help: "Config hot-reload attempts where at least one instance had not yet converged to the desired config version.",
	})

	// leaderObservationFailuresTotal counts reconciles where the elected leader
	// could not be observed over iproto (election/supervised modes only).
	leaderObservationFailuresTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "tarantool3_leader_observation_failures_total",
		Help: "Reconciles where the elected leader could not be observed over iproto.",
	})
)

func init() {
	metrics.Registry.MustRegister(renderTotal, reloadRetryTotal, leaderObservationFailuresTotal)
}

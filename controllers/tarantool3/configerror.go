package tarantool3

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tarantool/tarantool-operator/apis/v2alpha1"
)

// Config-rejection detection at startup (the static path). The operator
// delivers spec.config verbatim and lets Tarantool be the authority; when the
// config is invalid, Tarantool rejects it during box.cfg and the process exits
// BEFORE iproto comes up — so config:info() over iproto is unreachable and the
// reason lives only in the instance's log. The tarantool container runs with
// terminationMessagePolicy: FallbackToLogsOnError (see injectConfig), which
// places the crash-log tail into the pod's lastState.terminated.message; this
// detector reads it from pod status (no pods/log RBAC) and extracts the reason.
//
// It matches only Tarantool *configuration* error signatures, so an OOM kill,
// image-pull failure, or application panic is never mislabeled as a config
// error.

// tarantoolConfigErrorSignatures are substrings that identify a Tarantool
// configuration rejection in a crash-log line. A fatal config line ("F> ...
// config ...") is matched separately by lineIsConfigError.
var tarantoolConfigErrorSignatures = []string{
	"[cluster_config]",
	"Unexpected field",
	"is not allowed",
}

// lineIsConfigError reports whether a single log line names a config rejection.
func lineIsConfigError(line string) bool {
	for _, sig := range tarantoolConfigErrorSignatures {
		if strings.Contains(line, sig) {
			return true
		}
	}
	// Fatal config lines, e.g. "... F> ... config ...".
	return strings.Contains(line, "F>") && strings.Contains(line, "config")
}

// extractConfigRejection scans a crash-log tail (a container's
// lastState.terminated.message) and returns the first line that names a
// Tarantool config rejection, trimmed, or "" when none matches (or the input is
// empty). Returning a single line keeps the surfaced condition message bounded.
func extractConfigRejection(terminatedMessage string) string {
	if terminatedMessage == "" {
		return ""
	}
	for _, line := range strings.Split(terminatedMessage, "\n") {
		if lineIsConfigError(line) {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

// podConfigRejection returns a config-rejection reason for the pod's tarantool
// container, or "" if it is not crashing on a config error. It looks at the
// terminated state (current or last) with a non-zero exit code — Tarantool
// exits non-zero when it rejects the config — and matches the crash-log tail.
func podConfigRejection(pod *corev1.Pod) string {
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		// Restrict to the tarantool container when it is named; otherwise (a
		// single-container pod) any container is the instance, matching
		// tarantoolContainerIndex's fallback.
		if cs.Name != tarantoolContainerName && len(pod.Status.ContainerStatuses) > 1 {
			continue
		}
		for _, term := range []*corev1.ContainerStateTerminated{cs.State.Terminated, cs.LastTerminationState.Terminated} {
			if term == nil || term.ExitCode == 0 {
				continue
			}
			if reason := extractConfigRejection(term.Message); reason != "" {
				return reason
			}
		}
	}
	return ""
}

// detectConfigRejection lists this replica set's instance pods and returns the
// first config rejection found and the pod (instance) it came from, or "","".
// Best-effort: a List error yields no rejection so it never fails a reconcile.
// The whole set shares one config Secret, so the instances fail identically and
// the first reason is representative.
func (r *ReplicaSetReconciler) detectConfigRejection(ctx context.Context, rs *v2alpha1.ReplicaSet) (reason, instance string) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(rs.Namespace),
		client.MatchingLabels{ReplicaSetLabel: rs.Name}); err != nil {
		return "", ""
	}
	for i := range pods.Items {
		if reason := podConfigRejection(&pods.Items[i]); reason != "" {
			return reason, pods.Items[i].Name
		}
	}
	return "", ""
}

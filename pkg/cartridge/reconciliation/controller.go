package reconciliation

import (
	"github.com/tarantool/tarantool-operator/pkg/cartridge/election"
	"github.com/tarantool/tarantool-operator/pkg/cartridge/events"
	"github.com/tarantool/tarantool-operator/pkg/cartridge/k8s"
	"github.com/tarantool/tarantool-operator/pkg/cartridge/topology"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type Controller interface {
	client.Client

	GetScheme() *runtime.Scheme
	GetLeaderElection() *election.LeaderElection
	GetResourcesManager() k8s.ResourcesManager
	GetLabelsManager() k8s.LabelsManager
	GetEventsRecorder() *events.Recorder
	GetTopology() topology.CartridgeTopology
}

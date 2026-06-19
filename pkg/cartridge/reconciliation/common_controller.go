package reconciliation

import (
	"github.com/tarantool/tarantool-operator/pkg/cartridge/election"
	"github.com/tarantool/tarantool-operator/pkg/cartridge/events"
	"github.com/tarantool/tarantool-operator/pkg/cartridge/k8s"
	"github.com/tarantool/tarantool-operator/pkg/cartridge/topology"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type CommonController struct {
	client.Client

	Schema           *runtime.Scheme
	LeaderElection   *election.LeaderElection
	ResourcesManager k8s.ResourcesManager
	LabelsManager    k8s.LabelsManager
	EventsRecorder   *events.Recorder
	Topology         topology.CartridgeTopology
}

func (r *CommonController) GetLeaderElection() *election.LeaderElection {
	return r.LeaderElection
}

func (r *CommonController) GetResourcesManager() k8s.ResourcesManager {
	return r.ResourcesManager
}

func (r *CommonController) GetLabelsManager() k8s.LabelsManager {
	return r.LabelsManager
}

func (r *CommonController) GetEventsRecorder() *events.Recorder {
	return r.EventsRecorder
}

func (r *CommonController) GetScheme() *runtime.Scheme {
	return r.Schema
}

func (r *CommonController) GetTopology() topology.CartridgeTopology {
	return r.Topology
}

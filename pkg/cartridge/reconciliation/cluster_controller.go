package reconciliation

import (
	"context"

	"github.com/tarantool/tarantool-operator/pkg/cartridge/api"
)

type ClusterController interface {
	Controller
}

type ClusterWithRolesController[RolePhaseType comparable] interface {
	ClusterController

	IsAllRolesAtPhase(ctx context.Context, cluster api.Cluster, phase RolePhaseType) (bool, error)
}

type CommonClusterController struct {
	*CommonController
}

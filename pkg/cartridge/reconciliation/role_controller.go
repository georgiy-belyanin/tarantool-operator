package reconciliation

import (
	"github.com/tarantool/tarantool-operator/pkg/cartridge/api"
	"github.com/tarantool/tarantool-operator/pkg/cartridge/k8s"
)

type RoleController[RoleType api.Role] interface {
	Controller

	GetReplicasetsManger() k8s.ReplicasetsManger[RoleType]
}

type CommonRoleController struct {
	*CommonController
}

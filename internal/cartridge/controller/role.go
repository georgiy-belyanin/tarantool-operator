package controller

import (
	"github.com/tarantool/tarantool-operator/apis/cartridge/v1beta1"
	"github.com/tarantool/tarantool-operator/internal/cartridge/implementation"
	"github.com/tarantool/tarantool-operator/pkg/cartridge/k8s"
	"github.com/tarantool/tarantool-operator/pkg/cartridge/reconciliation"
)

type RoleController struct {
	*reconciliation.CommonRoleController

	ReplicasetsManger *implementation.ReplicasetsManger
}

func (r *RoleController) GetReplicasetsManger() k8s.ReplicasetsManger[*v1beta1.Role] {
	return r.ReplicasetsManger
}

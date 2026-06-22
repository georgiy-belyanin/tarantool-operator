package reconciliation

import (
	"github.com/tarantool/tarantool-operator/pkg/cartridge/api"
)

type ClusterContext[ClusterType api.Cluster] interface {
	Context

	SetCluster(cluster ClusterType)
	GetCluster() ClusterType
}

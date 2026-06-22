package controller

import (
	"github.com/tarantool/tarantool-operator/pkg/cartridge/reconciliation"
)

type CartridgeConfigController struct {
	*reconciliation.CommonCartridgeConfigController
}

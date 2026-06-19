package reconciliation

import (
	"github.com/tarantool/tarantool-operator/pkg/cartridge/api"
)

type CartridgeConfigContext[ConfigType api.CartridgeConfig] interface {
	Context

	SetCartridgeConfig(config ConfigType)
	GetCartridgeConfig() ConfigType
}

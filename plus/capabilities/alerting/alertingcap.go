package alertingcap

import (
	"plugin"

	"github.com/pohcee/procy-server/plus/validator"
	"github.com/pohcee/procy-server/share/logger"
)

const (
	InitAlertingServiceCapabilityEx = "InitAlertingServiceCapabilityEx"
)

type Capability struct {
	Provider CapabilityEx

	Config *Config
	*logger.Logger
}

func (cap *Capability) GetInitFuncName() (name string) {
	return InitAlertingServiceCapabilityEx
}

func (cap *Capability) InitProvider(sym plugin.Symbol) {
	fn := sym.(func(capability *Capability) (capProvider CapabilityEx))
	cap.Provider = fn(cap)
}

func (cap *Capability) GetAlertingCapabilityEx() (capEx CapabilityEx) {
	return cap.Provider
}

func (cap *Capability) GetConfigValidator() (v validator.Validator) {
	return nil
}

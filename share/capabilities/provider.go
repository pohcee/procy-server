package capabilities

import (
	"github.com/pohcee/procy-server/server/chconfig"
	chshare "github.com/pohcee/procy-server/share"
	"github.com/pohcee/procy-server/share/models"
)

func NewServerCapabilities(cfg *chconfig.MonitoringConfig) *models.Capabilities {
	caps := models.Capabilities{
		ServerVersion:      chshare.BuildVersion,
		MonitoringVersion:  chshare.MonitoringVersion,
		IPAddressesVersion: chshare.IPAddressesVersion,
	}

	if !cfg.Enabled {
		caps.MonitoringVersion = 0
	}
	return &caps
}

package chserver

import (
	"net/http"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/server/capabilities"
	chshare "github.com/openrport/openrport/share"
)

func (al *APIListener) handleGetStatus(w http.ResponseWriter, req *http.Request) {
	countActive := al.clientService.CountActive(req.Context())

	countDisconnected, err := al.clientService.CountDisconnected(req.Context())
	if err != nil {
		al.jsonErrorResponse(w, http.StatusInternalServerError, err)
		return
	}

	var twoFADelivery string
	if al.twoFASrv.MsgSrv != nil {
		twoFADelivery = al.twoFASrv.MsgSrv.DeliveryMethod()
	} else if al.config.API.TotPEnabled {
		twoFADelivery = "totp_authenticator_app"
	}

	response := api.NewSuccessPayload(map[string]interface{}{
		"version":                   chshare.BuildVersion,
		"clients_connected":         countActive,
		"clients_disconnected":      countDisconnected,
		"fingerprint":               al.fingerprint,
		"connect_url":               al.config.Server.URL,
		"pairing_url":               al.pairingURL(req),
		"pairing_builtin":           al.config.Pairing.Enabled,
		"clients_auth_source":       al.clientAuthProvider.Source(),
		"clients_auth_mode":         al.getClientsAuthMode(),
		"users_auth_source":         al.userService.GetProviderType(),
		"group_permissions_enabled": al.userService.SupportsGroupPermissions(),
		"two_fa_enabled":            al.config.API.IsTwoFAOn() || al.config.API.TotPEnabled,
		"two_fa_delivery_method":    twoFADelivery,
		"auditlog":                  al.auditLog.Status(),
		"auth_header":               al.config.API.AuthHeader != "",
		"tunnel_host":               al.config.Server.InternalTunnelProxyConfig.Host,
		"tunnel_proxy_enabled":      al.config.Server.InternalTunnelProxyConfig.Enabled,
		"caddy_integration_enabled": al.config.Caddy.Enabled,
		"excluded_ports":            al.config.Server.ExcludedPortsRaw,
		"used_ports":                al.config.Server.UsedPortsRaw,
		"monitoring_enabled":        al.config.Monitoring.Enabled,
		"password_min_length":       al.config.API.PasswordMinLength,
		"capabilities":              al.requestCapabilities(req),
	})

	al.writeJSONResponse(w, http.StatusOK, response)
}

// requestCapabilities returns statusCapabilities with those that depend on the requesting user.
func (al *APIListener) requestCapabilities(req *http.Request) capabilities.Capabilities {
	c := al.statusCapabilities()
	c.Operator = al.operatorAPI != nil && al.operatorAPI.IsOperator(req.Context())
	return c
}

// statusCapabilities returns the capabilities reported by the status endpoint.
func (al *APIListener) statusCapabilities() capabilities.Capabilities {
	c := capabilities.FromConfig(al.config)
	// a vault key provider that is not the passphrase needs no passphrase screens
	c.VaultPassphrase = al.vaultManager == nil || al.vaultManager.UsesPassphrase()
	// usage metering (Postgres with [metering] enabled)
	c.Usage = al.usage != nil
	// self-service signup (Postgres with [signup] enabled)
	c.Signup = al.signupAPI != nil
	if al.capabilitiesOverride != nil {
		c = al.capabilitiesOverride(c)
	}
	return c
}

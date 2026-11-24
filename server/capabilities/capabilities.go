// Package capabilities describes which features a server build offers, so API
// clients such as the web UI can hide what is not available.
package capabilities

import (
	rportplus "github.com/openrport/openrport/plus"
	"github.com/openrport/openrport/server/chconfig"
)

// Capabilities is reported as "capabilities" by GET /api/v1/status. A field is
// true when the feature is available on this server. Clients must treat a
// missing field (an older server) as available.
type Capabilities struct {
	// VaultPassphrase: the vault can be initialized and unlocked with a passphrase via the API. FromConfig
	// reports true; the status endpoint reports false when a vault key provider is injected (ServerOpts.VaultKeys).
	VaultPassphrase bool `json:"vault_passphrase"`
	// PortTunnels: raw TCP/UDP tunnels exposed on a server port.
	PortTunnels bool `json:"port_tunnels"`
	// HTTPProxyTunnels: HTTP(S) tunnels served through the built-in reverse proxy.
	HTTPProxyTunnels bool `json:"http_proxy_tunnels"`
	// SubdomainTunnels: tunnels published on their own subdomain via the Caddy integration.
	SubdomainTunnels bool `json:"subdomain_tunnels"`
	// BrowserVNC: VNC sessions in the browser (noVNC).
	BrowserVNC bool `json:"browser_vnc"`
	// BrowserRDP: RDP sessions in the browser (guacd).
	BrowserRDP bool `json:"browser_rdp"`
	// Monitoring: clients report metrics, processes and mountpoints.
	Monitoring bool `json:"monitoring"`
	// Alerting: alerting rules, templates and problems.
	Alerting bool `json:"alerting"`
	// Pairing: clients can be installed with a pairing code.
	Pairing bool `json:"pairing"`
	// FileUpload: files can be uploaded to clients.
	FileUpload bool `json:"file_upload"`
	// ScheduledJobs: commands and scripts can be scheduled.
	ScheduledJobs bool `json:"scheduled_jobs"`
	// Library: the script and command library.
	Library bool `json:"library"`
}

// FromConfig returns the capabilities offered by a server running with cfg.
// Features without a config switch are always available.
func FromConfig(cfg *chconfig.Config) Capabilities {
	proxy := cfg.Server.InternalTunnelProxyConfig
	return Capabilities{
		VaultPassphrase:  true,
		PortTunnels:      true,
		HTTPProxyTunnels: proxy.Enabled,
		SubdomainTunnels: cfg.Caddy.Enabled,
		BrowserVNC:       proxy.Enabled && proxy.NovncRoot != "",
		BrowserRDP:       proxy.Enabled && proxy.GuacdAddress != "",
		Monitoring:       cfg.Monitoring.Enabled,
		Alerting:         rportplus.IsPlusEnabled(cfg.PlusConfig),
		Pairing:          cfg.Pairing.Enabled || cfg.Server.PairingURL != "",
		FileUpload:       true,
		ScheduledJobs:    true,
		Library:          true,
	}
}

package chserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rportplus "github.com/openrport/openrport/plus"
	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/auditlog"
	"github.com/openrport/openrport/server/capabilities"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/clients"
	"github.com/openrport/openrport/server/clientsauth"
	"github.com/openrport/openrport/share/tenant"
)

func newStatusTestListener(cfg *chconfig.Config, override func(capabilities.Capabilities) capabilities.Capabilities) *APIListener {
	hour := time.Hour
	return &APIListener{
		Server: &Server{
			config:               cfg,
			clientService:        clients.NewClientService(nil, nil, clients.NewClientRepository(nil, &hour, testLog), testLog, nil),
			clientAuthProvider:   clientsauth.NewSingleProvider("client", "secret"),
			auditLog:             &auditlog.AuditLog{},
			capabilitiesOverride: override,
		},
		userService: users.NewAPIService(users.NewStaticProvider(nil), false, 0, -1),
	}
}

func getStatusData(t *testing.T, al *APIListener) map[string]json.RawMessage {
	t.Helper()
	rec := httptest.NewRecorder()
	al.handleGetStatus(rec, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil).WithContext(tenant.WithID(context.Background(), tenant.Default)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	return body.Data
}

func TestStatusCapabilities(t *testing.T) {
	testCases := []struct {
		name   string
		config func(c *chconfig.Config)
		want   string
	}{
		{
			name:   "default config",
			config: func(c *chconfig.Config) {},
			want: `{
				"vault_passphrase": true,
				"port_tunnels": true,
				"http_proxy_tunnels": false,
				"subdomain_tunnels": false,
				"browser_vnc": false,
				"browser_rdp": false,
				"monitoring": false,
				"alerting": false,
				"pairing": false,
				"file_upload": true,
				"scheduled_jobs": true,
				"library": true
			}`,
		},
		{
			name: "everything configured",
			config: func(c *chconfig.Config) {
				c.Server.InternalTunnelProxyConfig.Enabled = true
				c.Server.InternalTunnelProxyConfig.NovncRoot = "/var/lib/rportd/novnc"
				c.Server.InternalTunnelProxyConfig.GuacdAddress = "127.0.0.1:4822"
				c.Caddy.Enabled = true
				c.Monitoring.Enabled = true
				c.PlusConfig.PluginConfig = &rportplus.PluginConfig{PluginPath: "/usr/local/lib/rport/rport-plus.so"}
				c.Pairing.Enabled = true
			},
			want: `{
				"vault_passphrase": true,
				"port_tunnels": true,
				"http_proxy_tunnels": true,
				"subdomain_tunnels": true,
				"browser_vnc": true,
				"browser_rdp": true,
				"monitoring": true,
				"alerting": true,
				"pairing": true,
				"file_upload": true,
				"scheduled_jobs": true,
				"library": true
			}`,
		},
		{
			name: "browser VNC and RDP need the tunnel proxy",
			config: func(c *chconfig.Config) {
				c.Server.InternalTunnelProxyConfig.NovncRoot = "/var/lib/rportd/novnc"
				c.Server.InternalTunnelProxyConfig.GuacdAddress = "127.0.0.1:4822"
			},
			want: `{
				"vault_passphrase": true,
				"port_tunnels": true,
				"http_proxy_tunnels": false,
				"subdomain_tunnels": false,
				"browser_vnc": false,
				"browser_rdp": false,
				"monitoring": false,
				"alerting": false,
				"pairing": false,
				"file_upload": true,
				"scheduled_jobs": true,
				"library": true
			}`,
		},
		{
			name: "tunnel proxy without noVNC or guacd",
			config: func(c *chconfig.Config) {
				c.Server.InternalTunnelProxyConfig.Enabled = true
			},
			want: `{
				"vault_passphrase": true,
				"port_tunnels": true,
				"http_proxy_tunnels": true,
				"subdomain_tunnels": false,
				"browser_vnc": false,
				"browser_rdp": false,
				"monitoring": false,
				"alerting": false,
				"pairing": false,
				"file_upload": true,
				"scheduled_jobs": true,
				"library": true
			}`,
		},
		{
			name: "external pairing URL",
			config: func(c *chconfig.Config) {
				c.Server.PairingURL = "https://pairing.example.com"
			},
			want: `{
				"vault_passphrase": true,
				"port_tunnels": true,
				"http_proxy_tunnels": false,
				"subdomain_tunnels": false,
				"browser_vnc": false,
				"browser_rdp": false,
				"monitoring": false,
				"alerting": false,
				"pairing": true,
				"file_upload": true,
				"scheduled_jobs": true,
				"library": true
			}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &chconfig.Config{}
			tc.config(cfg)
			data := getStatusData(t, newStatusTestListener(cfg, nil))
			assert.JSONEq(t, tc.want, string(data["capabilities"]))
		})
	}
}

func TestStatusCapabilitiesOverride(t *testing.T) {
	cfg := &chconfig.Config{}
	cfg.Monitoring.Enabled = true

	var seen capabilities.Capabilities
	al := newStatusTestListener(cfg, func(computed capabilities.Capabilities) capabilities.Capabilities {
		seen = computed
		computed.VaultPassphrase = false
		computed.PortTunnels = false
		return computed
	})
	data := getStatusData(t, al)

	assert.True(t, seen.Monitoring, "the override receives the capabilities computed from the config")
	assert.JSONEq(t, `{
		"vault_passphrase": false,
		"port_tunnels": false,
		"http_proxy_tunnels": false,
		"subdomain_tunnels": false,
		"browser_vnc": false,
		"browser_rdp": false,
		"monitoring": true,
		"alerting": false,
		"pairing": false,
		"file_upload": true,
		"scheduled_jobs": true,
		"library": true
	}`, string(data["capabilities"]))
}

func TestStatusKeepsExistingFields(t *testing.T) {
	data := getStatusData(t, newStatusTestListener(&chconfig.Config{}, nil))
	for _, key := range []string{
		"version", "clients_connected", "clients_disconnected", "fingerprint", "connect_url",
		"pairing_url", "pairing_builtin", "clients_auth_source", "clients_auth_mode", "users_auth_source",
		"group_permissions_enabled", "two_fa_enabled", "two_fa_delivery_method", "auditlog", "auth_header",
		"tunnel_host", "tunnel_proxy_enabled", "caddy_integration_enabled", "excluded_ports", "used_ports",
		"monitoring_enabled", "password_min_length",
	} {
		assert.Contains(t, data, key)
	}
}

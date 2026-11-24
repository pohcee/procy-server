package chconfig

import (
	"runtime"
	"time"

	"github.com/spf13/viper"

	"github.com/openrport/openrport/server/api/message"
	auditlog "github.com/openrport/openrport/server/auditlog/config"
)

// The defaults rportd applies to its config file (SetDefaults).
const (
	DefaultKeepDisconnectedClients          = time.Hour
	DefaultPurgeDisconnectedClientsInterval = 1 * time.Minute
	DefaultCheckClientsConnectionInterval   = 5 * time.Minute
	DefaultCheckClientsConnectionTimeout    = 30 * time.Second
	DefaultMaxRequestBytes                  = 10 * 1024       // 10 KB
	DefaultMaxRequestBytesLibrary           = 256 * 1024      // 256KB
	DefaultMaxRequestBytesClient            = 512 * 1024      // 512KB
	DefaultMaxFilePushBytes                 = int64(10 << 20) // 10M
	DefaultCheckPortTimeout                 = 2 * time.Second
	DefaultUsedPorts                        = "20000-30000"
	DefaultExcludedPorts                    = "1-1024"
	DefaultServerAddress                    = "0.0.0.0:8080"
	DefaultLogLevel                         = "info"
	DefaultRunRemoteCmdTimeoutSec           = 60
	DefaultMonitoringDataStorageDuration    = "7d"
)

// DefaultMaxConcurrentSSHConnectionHandshakes is half the usable CPUs (at least 1).
var DefaultMaxConcurrentSSHConnectionHandshakes = calcMaxConcurrentSSHConnectionHandshakes()

func calcMaxConcurrentSSHConnectionHandshakes() (max int) {
	maxProcs := runtime.GOMAXPROCS(0)
	if maxProcs == 1 {
		return maxProcs
	}
	return maxProcs / 2
}

// SetDefaults sets rportd's defaults on v for every setting its config file may leave out.
func SetDefaults(v *viper.Viper) {
	v.SetDefault("logging.log_level", DefaultLogLevel)
	v.SetDefault("server.address", DefaultServerAddress)
	v.SetDefault("server.used_ports", []string{DefaultUsedPorts})
	v.SetDefault("server.excluded_ports", []string{DefaultExcludedPorts})
	v.SetDefault("server.data_dir", DefaultDataDirectory)
	v.SetDefault("server.sqlite_wal", true)
	v.SetDefault("server.keep_disconnected_clients", DefaultKeepDisconnectedClients)
	v.SetDefault("server.max_concurrent_ssh_handshakes", DefaultMaxConcurrentSSHConnectionHandshakes)
	v.SetDefault("server.purge_disconnected_clients_interval", DefaultPurgeDisconnectedClientsInterval)
	v.SetDefault("server.check_clients_connection_interval", DefaultCheckClientsConnectionInterval)
	v.SetDefault("server.check_clients_connection_timeout", DefaultCheckClientsConnectionTimeout)
	v.SetDefault("server.max_request_bytes_client", DefaultMaxRequestBytesClient)
	v.SetDefault("server.check_port_timeout", DefaultCheckPortTimeout)
	v.SetDefault("server.auth_write", true)
	v.SetDefault("server.auth_multiuse_creds", true)
	v.SetDefault("server.run_remote_cmd_timeout_sec", DefaultRunRemoteCmdTimeoutSec)
	v.SetDefault("server.client_login_wait", 2)
	v.SetDefault("server.max_failed_login", 5)
	v.SetDefault("pairing.enabled", false)
	v.SetDefault("pairing.code_ttl", DefaultPairingCodeTTL)
	v.SetDefault("server.ban_time", 3600)
	v.SetDefault("server.jobs_max_results", 10000)
	v.SetDefault("server.tls_min", "1.3")
	v.SetDefault("api.user_header", "Authentication-User")
	// Users created on the fly (header auth with create_missing_users) get no group unless configured.
	v.SetDefault("api.default_user_group", "")
	v.SetDefault("api.user_login_wait", 2)
	v.SetDefault("api.max_failed_login", 10)
	v.SetDefault("api.ban_time", 600)
	v.SetDefault("api.two_fa_token_ttl_seconds", 600)
	v.SetDefault("api.two_fa_send_timeout", 10*time.Second)
	v.SetDefault("api.two_fa_send_to_type", message.ValidationNone)
	v.SetDefault("api.enable_audit_log", true)
	v.SetDefault("api.totp_enabled", false)
	v.SetDefault("api.audit_log_rotation", auditlog.RotationMonthly)
	v.SetDefault("monitoring.data_storage_duration", DefaultMonitoringDataStorageDuration)
	v.SetDefault("monitoring.enabled", true)
	v.SetDefault("api.max_request_bytes", DefaultMaxRequestBytes)
	v.SetDefault("api.max_request_bytes_library", DefaultMaxRequestBytesLibrary)
	v.SetDefault("api.max_filepush_size", DefaultMaxFilePushBytes)
	v.SetDefault("api.enable_ws_test_endpoints", false)
	v.SetDefault("api.totp_login_session_ttl", time.Minute*10)
	v.SetDefault("api.totp_account_name", "RPort")
	v.SetDefault("api.password_min_length", 14)
	v.SetDefault("api.password_zxcvbn_minscore", 0)
	v.SetDefault("api.tls_min", "1.2")
	v.SetDefault("notifications.notification_script_dir", "/usr/local/lib/rport/notification_scripts")
}

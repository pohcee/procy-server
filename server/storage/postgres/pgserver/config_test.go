package pgserver

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/openrport/openrport/server/api/message"
	auditlog "github.com/openrport/openrport/server/auditlog/config"
	"github.com/openrport/openrport/server/chconfig"
	chshare "github.com/openrport/openrport/share"
	"github.com/openrport/openrport/share/logger"
)

// Test-only: builds a server config from a map of settings with rportd defaults (replaced by the node wiring).
const (
	EnvDatabaseURL = "TEST_NODE_DATABASE_URL" // required: Postgres URL of the application role
	EnvJWTSecret   = "TEST_NODE_JWT_SECRET"   // required: signs session tokens (shared by every instance)
	EnvAPIAddress  = "TEST_NODE_API_ADDRESS"  // default 127.0.0.1:3000
	EnvDocRoot     = "TEST_NODE_DOC_ROOT"     // the built web UI; empty serves the API only
	EnvDataDir     = "TEST_NODE_DATA_DIR"     // local scratch (uploads, fingerprint); default ./data
	EnvLogLevel    = "TEST_NODE_LOG_LEVEL"    // error, info (default) or debug
	EnvKeySeed     = "TEST_NODE_KEY_SEED"     // optional server key seed
	// EnvTrustedProxies lists the proxies (IPs or CIDRs, comma separated) whose X-Forwarded-For is believed, e.g.
	// the load balancer's ranges. Without it every request seems to come from the proxy, so failed logins in one
	// tenant would get the proxy's address banned for all tenants.
	EnvTrustedProxies = "TEST_NODE_TRUSTED_PROXIES"
)

// Placeholders that satisfy the config's "an auth source is configured" validation. The test server always injects the
// user and client credential stores, and clears these right after validation so they can never authenticate.
const validationOnlyAuth = "test-validation:not-a-credential"

// Settings is the test server own configuration, beside the server config.
type Settings struct {
	DatabaseURL string
}

// LoadConfig builds the server configuration from the environment, with rportd's defaults for everything the
// environment doesn't set.
func LoadConfig(getenv func(string) string) (*chconfig.Config, *Settings, error) {
	st := &Settings{DatabaseURL: getenv(EnvDatabaseURL)}
	if st.DatabaseURL == "" {
		return nil, nil, fmt.Errorf("%s is required", EnvDatabaseURL)
	}
	secret := getenv(EnvJWTSecret)
	if secret == "" {
		return nil, nil, fmt.Errorf("%s is required", EnvJWTSecret)
	}
	if len(secret) < 32 {
		return nil, nil, fmt.Errorf("%s must be at least 32 characters", EnvJWTSecret)
	}

	v := viper.New()
	setRportdDefaults(v)
	v.Set("api.address", orDefault(getenv(EnvAPIAddress), "127.0.0.1:3000"))
	v.Set("api.jwt_secret", secret)
	v.Set("api.doc_root", getenv(EnvDocRoot))
	v.Set("server.data_dir", orDefault(getenv(EnvDataDir), "./data"))
	v.Set("server.key_seed", getenv(EnvKeySeed))
	v.Set("logging.log_level", orDefault(getenv(EnvLogLevel), "info"))
	if tp := getenv(EnvTrustedProxies); tp != "" {
		var proxies []string
		for _, p := range strings.Split(tp, ",") {
			if p = strings.TrimSpace(p); p != "" {
				proxies = append(proxies, p)
			}
		}
		v.Set("api.trusted_proxies", proxies)
	}
	// devices never connect to the test server; pairing and the client-facing settings don't apply
	v.Set("pairing.enabled", false)
	v.Set("server.auth", validationOnlyAuth)
	v.Set("api.auth", validationOnlyAuth)

	cfg := &chconfig.Config{}
	if err := chshare.DecodeViperConfig(v, cfg, nil); err != nil {
		return nil, nil, err
	}
	mLog := logger.NewMemLogger()
	if err := cfg.ParseAndValidate(&mLog); err != nil {
		return nil, nil, fmt.Errorf("invalid configuration: %w", err)
	}
	cfg.Server.Auth = ""
	cfg.API.Auth = ""
	if cfg.Server.Auth != "" || cfg.API.Auth != "" {
		return nil, nil, errors.New("internal: placeholder credentials left in the configuration")
	}
	return cfg, st, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// setRportdDefaults applies the defaults rportd applies to its config file (cmd/rportd), so the test server server
// behaves like a standalone one where the test server doesn't say otherwise.
func setRportdDefaults(v *viper.Viper) {
	v.SetDefault("logging.log_level", "info")
	v.SetDefault("server.address", "127.0.0.1:0")
	v.SetDefault("server.used_ports", []string{"20000-30000"})
	v.SetDefault("server.excluded_ports", []string{"1-1024"})
	v.SetDefault("server.sqlite_wal", true)
	v.SetDefault("server.keep_disconnected_clients", time.Hour)
	v.SetDefault("server.max_concurrent_ssh_handshakes", 1)
	v.SetDefault("server.purge_disconnected_clients_interval", time.Minute)
	v.SetDefault("server.check_clients_connection_interval", 5*time.Minute)
	v.SetDefault("server.check_clients_connection_timeout", 30*time.Second)
	v.SetDefault("server.max_request_bytes_client", 512*1024)
	v.SetDefault("server.check_port_timeout", 2*time.Second)
	v.SetDefault("server.auth_write", true)
	v.SetDefault("server.auth_multiuse_creds", true)
	v.SetDefault("server.run_remote_cmd_timeout_sec", 60)
	v.SetDefault("server.client_login_wait", 2)
	v.SetDefault("server.max_failed_login", 5)
	v.SetDefault("pairing.code_ttl", chconfig.DefaultPairingCodeTTL)
	v.SetDefault("server.ban_time", 3600)
	v.SetDefault("server.jobs_max_results", 10000)
	v.SetDefault("server.tls_min", "1.3")
	v.SetDefault("api.user_header", "Authentication-User")
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
	v.SetDefault("monitoring.data_storage_duration", "7d")
	v.SetDefault("monitoring.enabled", true)
	v.SetDefault("api.max_request_bytes", 10*1024)
	v.SetDefault("api.max_request_bytes_library", 256*1024)
	v.SetDefault("api.max_filepush_size", int64(10<<20))
	v.SetDefault("api.enable_ws_test_endpoints", false)
	v.SetDefault("api.totp_login_session_ttl", 10*time.Minute)
	v.SetDefault("api.totp_account_name", "RPort")
	v.SetDefault("api.password_min_length", 14)
	v.SetDefault("api.password_zxcvbn_minscore", 0)
	v.SetDefault("api.tls_min", "1.2")
	v.SetDefault("notifications.notification_script_dir", "/usr/local/lib/rport/notification_scripts")
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

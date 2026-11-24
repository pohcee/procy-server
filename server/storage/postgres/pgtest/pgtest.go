// Package pgtest gives tests a real, migrated Postgres database.
//
// A package's tests call pgtest.Main from TestMain. The Postgres tests run only when RPORT_TEST_POSTGRES_URL is set,
// so the default test run needs neither a database nor network access:
//   - a superuser URL (postgres://…, e.g. a CI service container): the tests use that server;
//   - "embedded": the test binary starts an embedded Postgres (github.com/fergusstrange/embedded-postgres; its
//     binaries are downloaded once from Maven Central into ~/.cache/rport-embedded-postgres, then work offline);
//   - unset: New skips the test.
//
// "make test-postgres" runs them embedded. New gives each test its own database, migrated, with two connections: Owner (the schema
// owner, a superuser, which bypasses row-level security) and App (the role rport_app, which doesn't own the
// tables, so row-level security applies as in production).
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/migrate"
	"github.com/openrport/openrport/share/tenant"
)

// EnvDatabaseURL names a Postgres superuser URL to test against, or Embedded.
const EnvDatabaseURL = "RPORT_TEST_POSTGRES_URL"

// Embedded as the value of EnvDatabaseURL starts an embedded Postgres for the test binary.
const Embedded = "embedded"

// AppRole is the non-owner role App connects as.
const AppRole = "rport_app"

const appPassword = "rport_app"

var adminURL string

// Main runs the package's tests against Postgres; call it from TestMain: func TestMain(m *testing.M) { pgtest.Main(m) }.
func Main(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	adminURL = os.Getenv(EnvDatabaseURL)
	if adminURL == "" {
		return m.Run() // New skips
	}
	if adminURL == Embedded {
		stop, u, err := startEmbedded()
		if err != nil {
			fmt.Fprintf(os.Stderr, "pgtest: %v\n", err)
			return 1
		}
		defer stop()
		adminURL = u
	}
	if err := ensureAppRole(); err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: %v\n", err)
		return 1
	}
	return m.Run()
}

// StartEmbedded starts an embedded Postgres on a free local port and returns its superuser URL and a stop function.
// For tests and local development (see pgtest/devdb).
func StartEmbedded() (stop func(), url string, err error) { return startEmbedded() }

// EnsureAppRole creates the AppRole login role (password "rport_app") on the server at adminURL if it is missing.
func EnsureAppRole(adminURL string) error { return ensureAppRoleAt(adminURL) }

func startEmbedded() (func(), string, error) {
	cache := os.Getenv("RPORT_EMBEDDED_POSTGRES_CACHE")
	if cache == "" {
		home, err := os.UserCacheDir()
		if err != nil {
			return nil, "", err
		}
		cache = filepath.Join(home, "rport-embedded-postgres")
	}
	if err := os.MkdirAll(cache, 0o755); err != nil {
		return nil, "", err
	}
	runtime, err := os.MkdirTemp("", "rport-pgtest-")
	if err != nil {
		return nil, "", err
	}
	port, err := freePort()
	if err != nil {
		return nil, "", err
	}
	cfg := embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V17).
		Port(uint32(port)).
		Username("postgres").Password("postgres").Database("postgres").
		CachePath(cache).
		RuntimePath(runtime).
		Logger(nil)
	ep := embeddedpostgres.NewDatabase(cfg)

	// test binaries of several packages start at once; only one may download into the shared cache
	unlock, err := lockFile(filepath.Join(cache, ".lock"))
	if err != nil {
		return nil, "", err
	}
	err = ep.Start()
	unlock()
	if err != nil {
		_ = os.RemoveAll(runtime)
		return nil, "", fmt.Errorf("start embedded postgres: %w", err)
	}
	stop := func() {
		_ = ep.Stop()
		_ = os.RemoveAll(runtime)
	}
	return stop, fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/postgres?sslmode=disable", port), nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func ensureAppRole() error { return ensureAppRoleAt(adminURL) }

func ensureAppRoleAt(adminURL string) error {
	ctx := context.Background()
	db, err := postgres.Open(ctx, adminURL)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.ExecContext(ctx, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '`+AppRole+`') THEN
			CREATE ROLE `+AppRole+` LOGIN PASSWORD '`+appPassword+`' NOSUPERUSER NOBYPASSRLS;
		END IF;
	END $$`)
	return err
}

// DB is one test's database.
type DB struct {
	Name  string
	Owner *sqlx.DB // schema owner (superuser): bypasses row-level security; use for setup only
	App   *sqlx.DB // rport_app: what a server connects as; row-level security applies
	// AppURL and OwnerURL are the connection URLs of App and Owner, for code under test that opens its own.
	AppURL, OwnerURL string
}

// New creates a fresh, migrated database for t (dropped at the end of the test) in which the tenants given exist.
func New(t testing.TB, tenants ...tenant.ID) *DB {
	t.Helper()
	if os.Getenv(EnvDatabaseURL) == "" {
		t.Skip("Postgres test: set " + EnvDatabaseURL + " to a superuser URL or to \"embedded\" to run it")
	}
	if adminURL == "" {
		t.Fatal("pgtest: call pgtest.Main from TestMain")
	}
	ctx := context.Background()
	admin, err := postgres.Open(ctx, adminURL)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "t_" + hex.EncodeToString(b)
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close()
	})

	ownerURL := withDatabase(t, adminURL, name, "", "")
	owner, err := postgres.Open(ctx, ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { owner.Close() })
	if _, err := migrate.Up(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := migrate.GrantApp(ctx, owner, AppRole); err != nil {
		t.Fatal(err)
	}
	appURL := withDatabase(t, adminURL, name, AppRole, appPassword)
	app, err := postgres.Open(ctx, appURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })

	d := &DB{Name: name, Owner: owner, App: app, AppURL: appURL, OwnerURL: ownerURL}
	d.AddTenants(t, tenants...)
	return d
}

// AddTenants creates the given tenants (existing ones are left alone).
func (d *DB) AddTenants(t testing.TB, ids ...tenant.ID) {
	t.Helper()
	for _, id := range ids {
		if _, err := d.Owner.Exec("INSERT INTO tenants (id, name) VALUES ($1, $1) ON CONFLICT (id) DO NOTHING", string(id)); err != nil {
			t.Fatal(err)
		}
	}
}

func withDatabase(t testing.TB, raw, db, user, password string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("pgtest: %s must be a postgres:// URL: %v", EnvDatabaseURL, err)
	}
	u.Path = "/" + db
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	return u.String()
}

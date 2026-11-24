package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"

	chserver "github.com/openrport/openrport/server"
	"github.com/openrport/openrport/server/storage/postgres/migrate"
	"github.com/openrport/openrport/server/storage/postgres/pgserver"
	"github.com/openrport/openrport/share/logger"
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "migrate the databases and exit",
	Long: "Migrate every SQLite database in the data directory to the latest version, then exit. " +
		"rportd also migrates them at startup; run this first when the server is started with migrations skipped. " +
		"The settings, data_dir included, come from the config file (-c) only: server flags such as --data-dir " +
		"are not accepted here.\n\n" +
		"With [storage] backend = \"postgres\" it migrates the Postgres schema instead, connected as the schema " +
		"owner (--dsn, default [storage] dsn), and with --grant-to gives the application role the server connects " +
		"as its privileges. A Postgres node never migrates at startup.",
	Example: "rportd migrate -c /etc/rport/rportd.conf\n" +
		"rportd migrate -c /etc/rport/rportd.conf --dsn postgres://rport_owner@db/rport --grant-to rport_app",
	Run: func(*cobra.Command, []string) {
		out := logger.NewLogger("migrate", logger.LogOutput{File: os.Stdout}, logger.LogLevelInfo)
		if err := runMigrate(out); err != nil {
			log.Fatal(err)
		}
	},
}

var (
	migrateDSN     *string
	migrateGrantTo *string
)

// runMigrate reads and validates the config, refuses to run as root unless allowed (as the server does) and
// migrates the databases.
func runMigrate(out *logger.Logger) error {
	mLog := logger.NewMemLogger()
	if err := loadCommandConfig(&mLog); err != nil {
		return fmt.Errorf("invalid config: %w. See rportd --help", err)
	}
	if rootRefused(cfg) {
		return errors.New(rootRefusedMsg)
	}
	if !cfg.Storage.IsPostgres() {
		if *migrateDSN != "" || *migrateGrantTo != "" {
			return errors.New("--dsn and --grant-to are used " + notPostgresMsg)
		}
		return chserver.MigrateSQLite(cfg, out)
	}
	return migratePostgres(context.Background(), out, *migrateDSN, *migrateGrantTo)
}

func migratePostgres(ctx context.Context, out *logger.Logger, dsn, grantTo string) error {
	db, err := pgserver.OpenOwner(ctx, cfg, dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	applied, err := migrate.Up(ctx, db)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		out.Infof("Postgres: at the latest version, nothing to migrate")
	} else {
		out.Infof("Postgres: applied %d migration(s)", len(applied))
	}
	if grantTo != "" {
		if err := migrate.GrantApp(ctx, db, grantTo); err != nil {
			return err
		}
		out.Infof("Postgres: granted the application privileges to %s", grantTo)
	}
	return nil
}

func init() {
	RootCmd.AddCommand(migrateCmd)

	migrateDSN = migrateCmd.Flags().String("dsn", "", "Postgres: connection URL of the schema owner (default [storage] dsn)")
	migrateGrantTo = migrateCmd.Flags().String("grant-to", "", "Postgres: give this role the privileges the server needs")

	// reset default usage func
	migrateCmd.SetUsageFunc((&cobra.Command{}).UsageFunc())
}

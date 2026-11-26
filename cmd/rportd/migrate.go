package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"

	chserver "github.com/pohcee/procy-server/server"
	"github.com/pohcee/procy-server/server/storage/postgres/clientauth"
	"github.com/pohcee/procy-server/server/storage/postgres/migrate"
	"github.com/pohcee/procy-server/server/storage/postgres/pgserver"
	"github.com/pohcee/procy-server/share/logger"
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
		"as its privileges; --grant-device-to gives the device role (nodes with [storage] role = \"device\") its " +
		"narrow privileges: lookup and ingest functions only, no table access. It also stores every client " +
		"credential password that is not hashed yet as its argon2id hash. A Postgres node never migrates at startup.",
	Example: "rportd migrate -c /etc/rport/rportd.conf\n" +
		"rportd migrate -c /etc/rport/rportd.conf --dsn postgres://rport_owner@db/rport --grant-to rport_app " +
		"--grant-device-to rport_device",
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
	migrateDevice  *string
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
		if *migrateDSN != "" || *migrateGrantTo != "" || *migrateDevice != "" {
			return errors.New("--dsn, --grant-to and --grant-device-to are used " + notPostgresMsg)
		}
		return chserver.MigrateSQLite(cfg, out)
	}
	return migratePostgres(context.Background(), out, *migrateDSN, *migrateGrantTo, *migrateDevice)
}

func migratePostgres(ctx context.Context, out *logger.Logger, dsn, grantTo, grantDevice string) error {
	if grantTo != "" && grantTo == grantDevice {
		return errors.New("--grant-to and --grant-device-to name the same role: the device role must be a separate login")
	}
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
	if grantDevice != "" {
		if err := migrate.GrantDevice(ctx, db, grantDevice); err != nil {
			return err
		}
		out.Infof("Postgres: granted the device privileges to %s", grantDevice)
	}
	hashed, err := clientauth.HashStored(ctx, db)
	if err != nil {
		return err
	}
	if hashed > 0 {
		out.Infof("Postgres: stored %d client credential password(s) as argon2id hashes", hashed)
	}
	return nil
}

func init() {
	RootCmd.AddCommand(migrateCmd)

	migrateDSN = migrateCmd.Flags().String("dsn", "", "Postgres: connection URL of the schema owner (default [storage] dsn)")
	migrateGrantTo = migrateCmd.Flags().String("grant-to", "", "Postgres: give this role the privileges the server needs")
	migrateDevice = migrateCmd.Flags().String("grant-device-to", "", "Postgres: give this role the privileges of a device process ([storage] role = \"device\")")

	// reset default usage func
	migrateCmd.SetUsageFunc((&cobra.Command{}).UsageFunc())
}

package main

import (
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"

	chserver "github.com/openrport/openrport/server"
	"github.com/openrport/openrport/share/logger"
)

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "migrate the databases and exit",
	Long: "Migrate every SQLite database in the data directory to the latest version, then exit. " +
		"rportd also migrates them at startup; run this first when the server is started with migrations skipped. " +
		"The settings, data_dir included, come from the config file (-c) only: server flags such as --data-dir " +
		"are not accepted here.",
	Example: "rportd migrate -c /etc/rport/rportd.conf",
	Run: func(*cobra.Command, []string) {
		out := logger.NewLogger("migrate", logger.LogOutput{File: os.Stdout}, logger.LogLevelInfo)
		if err := runMigrate(out); err != nil {
			log.Fatal(err)
		}
	},
}

// runMigrate reads and validates the config, refuses to run as root unless allowed (as the server does) and
// migrates the databases.
func runMigrate(out *logger.Logger) error {
	mLog := logger.NewMemLogger()
	if err := decodeAndValidateConfig(&mLog); err != nil {
		return fmt.Errorf("invalid config: %w. See rportd --help", err)
	}
	if rootRefused(cfg) {
		return errors.New(rootRefusedMsg)
	}
	return chserver.MigrateSQLite(cfg, out)
}

func init() {
	RootCmd.AddCommand(migrateCmd)

	// reset default usage func
	migrateCmd.SetUsageFunc((&cobra.Command{}).UsageFunc())
}

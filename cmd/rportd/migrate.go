package main

import (
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
		"rportd also migrates them at startup; run this first when the server is started with migrations skipped.",
	Example: "rportd migrate -c /etc/rport/rportd.conf",
	Run: func(*cobra.Command, []string) {
		mLog := logger.NewMemLogger()
		err := decodeAndValidateConfig(&mLog)
		if err != nil {
			log.Fatalf("Invalid config: %v. See rportd --help", err)
		}

		out := logger.NewLogger("migrate", logger.LogOutput{File: os.Stdout}, logger.LogLevelInfo)
		if err := chserver.MigrateSQLite(cfg, out); err != nil {
			log.Fatal(err)
		}
	},
}

func init() {
	RootCmd.AddCommand(migrateCmd)

	// reset default usage func
	migrateCmd.SetUsageFunc((&cobra.Command{}).UsageFunc())
}

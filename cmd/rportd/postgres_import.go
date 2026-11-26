package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/pohcee/procy-server/server/storage/importer"
	"github.com/pohcee/procy-server/server/storage/postgres/pgserver"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/tenant"
)

// EnvVaultPassphrase holds the passphrase of the source vault for "rportd import".
const EnvVaultPassphrase = "RPORT_VAULT_PASSPHRASE" //nolint:gosec // G101: an environment variable name

var (
	importCmd = &cobra.Command{
		Use:   "import",
		Short: "import a standalone data directory into Postgres (preview)",
		Long: "Copy the data of a stopped standalone server (its data directory and, with --from-config, the users and " +
			"client credentials of its auth settings) into one tenant of this server's Postgres database ([storage] " +
			"backend = \"postgres\", from -c). The source is only read, never changed. Everything is copied in one " +
			"transaction, and rows already present are kept, so the import can be run again. History (jobs, audit " +
			"log, monitoring, notification log) is copied only with --with-history.\n\n" +
			"The source vault values are decrypted with the source vault passphrase (from $" + EnvVaultPassphrase +
			", or the first line of stdin with --vault-passphrase-stdin) and encrypted again with the tenant's key " +
			"([vault] key_provider = \"kms\" or \"local\", required on Postgres).",
		Example: "rportd import -c /etc/rport/rportd.conf --from /var/lib/rport --from-config /etc/rport/old-rportd.conf --dry-run",
		Args:    cobra.NoArgs,
		Run: func(*cobra.Command, []string) {
			if err := runImport(context.Background(), os.Stdin, os.Stdout, os.Stderr); err != nil {
				log.Fatal(err)
			}
		},
	}

	importFrom, importFromConfig, importTenant, importDSN      *string
	importDryRun, importWithHistory, importPassphraseFromStdin *bool
)

func runImport(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	if *importFrom == "" && *importFromConfig == "" {
		return errors.New("--from (the standalone data directory) or --from-config is required")
	}
	mLog := logger.NewMemLogger()
	if err := loadCommandConfig(&mLog); err != nil {
		return fmt.Errorf("invalid config: %w. See rportd --help", err)
	}
	if !cfg.Storage.IsPostgres() {
		return errors.New(`rportd import needs [storage] backend = "postgres" in the config (-c) of the server to import into`)
	}
	if rootRefused(cfg) {
		return errors.New(rootRefusedMsg)
	}
	tid := tenant.ID(*importTenant)
	if tid == "" {
		tid = tenant.Default
	}

	passphrase := os.Getenv(EnvVaultPassphrase)
	if passphrase == "" && *importPassphraseFromStdin {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read the vault passphrase from stdin: %w", err)
		}
		passphrase = strings.TrimRight(line, "\r\n")
	}

	db, err := pgserver.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	if *importDSN != "" && !*importDryRun {
		if err := ensureTenant(ctx, tid, stdout); err != nil {
			return err
		}
	}

	keys, closeKeys, err := pgserver.VaultKeys(ctx, cfg, db, stderr)
	if err != nil {
		return err
	}
	defer closeKeys()

	rep, err := importer.Run(ctx, db, importer.Options{
		From:            *importFrom,
		FromConfig:      *importFromConfig,
		Tenant:          tid,
		DryRun:          *importDryRun,
		WithHistory:     *importWithHistory,
		VaultPassphrase: passphrase,
		VaultKeys:       keys,
		Log:             stderr,
	})
	if err != nil {
		return err
	}
	rep.Print(stdout)
	return nil
}

// ensureTenant creates the tenant as the schema owner (--dsn) when it does not exist yet.
func ensureTenant(ctx context.Context, tid tenant.ID, stdout io.Writer) error {
	owner, err := pgserver.OpenOwner(ctx, cfg, *importDSN)
	if err != nil {
		return err
	}
	defer owner.Close()
	added, err := pgserver.EnsureTenant(ctx, owner, tid)
	if err != nil || !added {
		return err
	}
	fmt.Fprintf(stdout, "added tenant %q\n", tid)
	return nil
}

func init() {
	RootCmd.AddCommand(importCmd)
	f := importCmd.Flags()
	importFrom = f.String("from", "", "data directory of the stopped standalone server (default: data_dir of --from-config)")
	importFromConfig = f.String("from-config", "", "rportd.conf of the standalone server: its users and client credentials are imported too")
	importTenant = f.String("tenant", string(tenant.Default), "tenant to import into")
	importDSN = f.String("dsn", "", "connection URL of the schema owner: creates the tenant if it does not exist")
	importDryRun = f.Bool("dry-run", false, "read and check everything and print the counts, but write nothing")
	importWithHistory = f.Bool("with-history", false, "also import jobs, the audit log, monitoring data and the notification log")
	importPassphraseFromStdin = f.Bool("vault-passphrase-stdin", false, "read the source vault passphrase from the first line of stdin (default $"+EnvVaultPassphrase+")")

	// reset default usage func
	importCmd.SetUsageFunc((&cobra.Command{}).UsageFunc())
}

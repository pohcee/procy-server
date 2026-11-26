package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/spf13/cobra"

	"github.com/pohcee/procy-server/server/storage/postgres/pgserver"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/tenant"
)

var (
	tenantCmd = &cobra.Command{
		Use:   "tenant",
		Short: "manage tenants (Postgres storage)",
		Long: "Manage the tenants (organizations) of a server with [storage] backend = \"postgres\". " +
			"Each tenant has its own users, clients and data.",
	}
	tenantAddCmd = &cobra.Command{
		Use:   "add ID [NAME]",
		Short: "add a tenant",
		Long: "Add a tenant with the built-in library entries. Tenants are created by the schema owner (--dsn, " +
			"default [storage] dsn): the application role may not write the tenants table.",
		Example: "rportd tenant add acme \"Acme Corp\" -c /etc/rport/rportd.conf --dsn postgres://rport_owner@db/rport",
		Args:    cobra.RangeArgs(1, 2),
		Run: func(_ *cobra.Command, args []string) {
			out := logger.NewLogger("tenant", logger.LogOutput{File: os.Stdout}, logger.LogLevelInfo)
			if err := runTenantAdd(out, args); err != nil {
				log.Fatal(err)
			}
		},
	}

	tenantDSN *string
)

// runTenantAdd adds the tenant args[0], named args[1] (default: its id).
func runTenantAdd(out *logger.Logger, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("expected ID [NAME]")
	}
	mLog := logger.NewMemLogger()
	if err := loadCommandConfig(&mLog); err != nil {
		return fmt.Errorf("invalid config: %w. See rportd --help", err)
	}
	if !cfg.Storage.IsPostgres() {
		return errors.New(`tenants exist only with [storage] backend = "postgres"`)
	}
	if rootRefused(cfg) {
		return errors.New(rootRefusedMsg)
	}
	id := tenant.ID(args[0])
	if id == "" {
		return errors.New("the tenant id must not be empty")
	}
	name := ""
	if len(args) == 2 {
		name = args[1]
	}
	ctx := context.Background()
	db, err := pgserver.OpenOwner(ctx, cfg, *tenantDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := pgserver.AddTenant(ctx, db, id, name); err != nil {
		if *tenantDSN == "" {
			return fmt.Errorf("%w (tenants are added by the schema owner: pass its connection URL with --dsn)", err)
		}
		return err
	}
	out.Infof("added tenant %q", id)
	return nil
}

func init() {
	RootCmd.AddCommand(tenantCmd)
	tenantCmd.AddCommand(tenantAddCmd)

	tenantDSN = tenantAddCmd.Flags().String("dsn", "", "connection URL of the schema owner (default [storage] dsn)")

	// reset default usage func
	tenantCmd.SetUsageFunc((&cobra.Command{}).UsageFunc())
}

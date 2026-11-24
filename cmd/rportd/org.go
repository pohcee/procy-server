package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/user"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/openrport/openrport/server/operator"
	"github.com/openrport/openrport/server/storage/postgres/pgserver"
	"github.com/openrport/openrport/share/logger"
)

var (
	orgCmd = &cobra.Command{
		Use:   "org",
		Short: "manage organizations as a platform operator (Postgres storage)",
		Long: "List, add, suspend and reactivate the organizations (tenants) of a server with [storage] backend = " +
			"\"postgres\", set their device limits and add their administrators. Connects with --dsn (the schema " +
			"owner or the application role; default [storage] dsn). Every change is written to the audit log of the " +
			"system organization \"" + string(operator.SystemTenant) + "\", whose administrators are the platform operators.",
	}
	orgListCmd = &cobra.Command{
		Use:   "list",
		Short: "list the organizations",
		Args:  cobra.NoArgs,
		Run:   orgRun(runOrgList),
	}
	orgAddCmd = &cobra.Command{
		Use:     "add ID [NAME]",
		Short:   "add an organization",
		Example: "rportd org add acme \"Acme Corp\" --device-limit 50 -c /etc/rport/rportd.conf",
		Args:    cobra.RangeArgs(1, 2),
		Run:     orgRun(runOrgAdd),
	}
	orgSuspendCmd = &cobra.Command{
		Use:   "suspend ID",
		Short: "suspend an organization: its users can't log in and their sessions and API tokens stop working",
		Args:  cobra.ExactArgs(1),
		Run:   orgRun(runOrgSuspend),
	}
	orgActivateCmd = &cobra.Command{
		Use:   "activate ID",
		Short: "reactivate a suspended organization",
		Args:  cobra.ExactArgs(1),
		Run:   orgRun(runOrgActivate),
	}
	orgSetLimitCmd = &cobra.Command{
		Use:     "set-limit ID LIMIT",
		Short:   "set the most devices an organization may have (a number, or \"unlimited\")",
		Example: "rportd org set-limit acme 100\nrportd org set-limit acme unlimited",
		Args:    cobra.ExactArgs(2),
		Run:     orgRun(runOrgSetLimit),
	}
	orgAddAdminCmd = &cobra.Command{
		Use:   "add-admin ID USERNAME",
		Short: "add an administrator to an organization; prints a generated password once",
		Long: "Add a user to the organization's Administrators group with a generated password, printed once. The " +
			"password must be changed at the first login. With the ID \"" + string(operator.SystemTenant) + "\" it " +
			"adds a platform operator.",
		Example: "rportd org add-admin acme alice@example.com\nrportd org add-admin " + string(operator.SystemTenant) + " ops@example.com",
		Args:    cobra.ExactArgs(2),
		Run:     orgRun(runOrgAddAdmin),
	}

	orgDSN         *string
	orgDeviceLimit *string
	orgTwoFASendTo *string
	orgJSON        *bool
)

type orgCommand func(ctx context.Context, w io.Writer, svc operator.Service, by operator.Actor, args []string) error

func orgRun(fn orgCommand) func(*cobra.Command, []string) {
	return func(_ *cobra.Command, args []string) {
		if err := runOrg(context.Background(), os.Stdout, fn, args); err != nil {
			log.Fatal(err)
		}
	}
}

// runOrg loads the config, connects and runs fn with the operator service.
func runOrg(ctx context.Context, w io.Writer, fn orgCommand, args []string) error {
	mLog := logger.NewMemLogger()
	if err := loadCommandConfig(&mLog); err != nil {
		return fmt.Errorf("invalid config: %w. See rportd --help", err)
	}
	if !cfg.Storage.IsPostgres() {
		return errors.New(`organizations exist only with [storage] backend = "postgres"`)
	}
	if rootRefused(cfg) {
		return errors.New(rootRefusedMsg)
	}
	db, err := pgserver.OpenOwner(ctx, cfg, *orgDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	return fn(ctx, w, pgserver.NewOperator(db, cfg), cliActor(), args)
}

// cliActor is who the audit log names for a command: the operating system user.
func cliActor() operator.Actor {
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	return operator.Actor{Username: "cli:" + name}
}

func parseLimit(s string) (*int, error) {
	if s == "" || strings.EqualFold(s, "unlimited") || strings.EqualFold(s, "none") {
		return nil, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return nil, fmt.Errorf("the device limit must be a whole number >= 0 or \"unlimited\", not %q", s)
	}
	return &n, nil
}

func formatLimit(l *int) string {
	if l == nil {
		return "unlimited"
	}
	return strconv.Itoa(*l)
}

func runOrgList(ctx context.Context, w io.Writer, svc operator.Service, _ operator.Actor, _ []string) error {
	orgs, err := svc.List(ctx)
	if err != nil {
		return err
	}
	if *orgJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(orgs)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATUS\tDEVICES\tLIMIT\tUSERS\tCREATED")
	for _, o := range orgs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%d\t%s\n", o.ID, o.Name, o.Status, o.DeviceCount, formatLimit(o.DeviceLimit),
			o.UserCount, o.CreatedAt.UTC().Format("2006-01-02 15:04"))
	}
	return tw.Flush()
}

func printOrg(w io.Writer, verb string, o *operator.Organization) {
	fmt.Fprintf(w, "%s organization %q (%s): status %s, device limit %s, %d device(s), %d user(s)\n",
		verb, o.ID, o.Name, o.Status, formatLimit(o.DeviceLimit), o.DeviceCount, o.UserCount)
}

func runOrgAdd(ctx context.Context, w io.Writer, svc operator.Service, by operator.Actor, args []string) error {
	limit, err := parseLimit(*orgDeviceLimit)
	if err != nil {
		return err
	}
	in := operator.NewOrganization{ID: args[0], DeviceLimit: limit}
	if len(args) > 1 {
		in.Name = args[1]
	}
	o, err := svc.Create(ctx, by, in)
	if err != nil {
		return err
	}
	printOrg(w, "added", o)
	return nil
}

func runOrgSuspend(ctx context.Context, w io.Writer, svc operator.Service, by operator.Actor, args []string) error {
	o, err := svc.SetStatus(ctx, by, args[0], operator.StatusSuspended)
	if err != nil {
		return err
	}
	printOrg(w, "suspended", o)
	return nil
}

func runOrgActivate(ctx context.Context, w io.Writer, svc operator.Service, by operator.Actor, args []string) error {
	o, err := svc.SetStatus(ctx, by, args[0], operator.StatusActive)
	if err != nil {
		return err
	}
	printOrg(w, "activated", o)
	return nil
}

func runOrgSetLimit(ctx context.Context, w io.Writer, svc operator.Service, by operator.Actor, args []string) error {
	limit, err := parseLimit(args[1])
	if err != nil {
		return err
	}
	o, err := svc.SetDeviceLimit(ctx, by, args[0], limit)
	if err != nil {
		return err
	}
	printOrg(w, "updated", o)
	return nil
}

func runOrgAddAdmin(ctx context.Context, w io.Writer, svc operator.Service, by operator.Actor, args []string) error {
	a, err := svc.AddAdmin(ctx, by, args[0], operator.NewAdmin{Username: args[1], TwoFASendTo: *orgTwoFASendTo})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "added administrator %q to %q\npassword (shown once; it must be changed at the first login): %s\n",
		a.Username, a.Organization, a.Password)
	return nil
}

func init() {
	RootCmd.AddCommand(orgCmd)
	orgCmd.AddCommand(orgListCmd, orgAddCmd, orgSuspendCmd, orgActivateCmd, orgSetLimitCmd, orgAddAdminCmd)

	orgDSN = orgCmd.PersistentFlags().String("dsn", "", "connection URL (schema owner or application role; default [storage] dsn)")
	orgDeviceLimit = orgAddCmd.Flags().String("device-limit", "", "the most devices the organization may have (default unlimited)")
	orgTwoFASendTo = orgAddAdminCmd.Flags().String("two-fa-send-to", "", "where 2FA tokens are sent (required when 2FA is on)")
	orgJSON = orgListCmd.Flags().Bool("json", false, "print JSON")

	// reset default usage func
	orgCmd.SetUsageFunc((&cobra.Command{}).UsageFunc())
}

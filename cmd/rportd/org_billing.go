package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/openrport/openrport/server/billing"
	pgbilling "github.com/openrport/openrport/server/storage/postgres/billing"
	"github.com/openrport/openrport/server/storage/postgres/pgserver"
	"github.com/openrport/openrport/share/logger"
)

var (
	orgSetPlanCmd = &cobra.Command{
		Use:     "set-plan ID PLAN",
		Short:   "put an organization on a plan of the plans file ([billing] enabled); its device limit follows the plan",
		Example: "rportd org set-plan acme team",
		Args:    cobra.ExactArgs(2),
		Run:     billingRun(runOrgSetPlan),
	}
	orgSetBillingCmd = &cobra.Command{
		Use:   "set-billing ID STATUS",
		Short: "set the billing status of an organization: paying, trial, exempt or comped ([billing] enabled)",
		Long: "Set the billing status of an organization. trial needs --trial-ends (a date or RFC 3339 time), comped " +
			"needs --comped-until (the last day not charged), exempt needs --reason and takes --exempt-limits " +
			"(\"plan\", the default, or \"none\"). Exempt organizations are metered and never charged.",
		Example: "rportd org set-billing acme trial --trial-ends 2026-12-08\n" +
			"rportd org set-billing partner exempt --reason \"reseller partner\" --exempt-limits none\n" +
			"rportd org set-billing acme comped --comped-until 2027-03-31 --reason \"launch customer\"",
		Args: cobra.ExactArgs(2),
		Run:  billingRun(runOrgSetBilling),
	}
	orgBillingCmd = &cobra.Command{
		Use:   "billing [ID]",
		Short: "show the billing of every organization, or of one with its closed periods ([billing] enabled)",
		Args:  cobra.RangeArgs(0, 1),
		Run:   billingRun(runOrgBilling),
	}
	orgLiftCapCmd = &cobra.Command{
		Use:     "lift-cap ID",
		Short:   "lift an organization's hard transfer cap until a time, or raise this month's allowance ([billing] enabled); what is not given is kept",
		Example: "rportd org lift-cap acme --until 2026-12-01\nrportd org lift-cap acme --extra-transfer 50GiB\nrportd org lift-cap acme --end",
		Args:    cobra.ExactArgs(1),
		Run:     billingRun(runOrgLiftCap),
	}
	orgApprovePeriodCmd = &cobra.Command{
		Use:     "approve-period ID YYYY-MM",
		Short:   "release a period held for overage review so it is charged ([billing] enabled)",
		Example: "rportd org approve-period acme 2026-11",
		Args:    cobra.ExactArgs(2),
		Run:     billingRun(runOrgApprovePeriod),
	}

	orgTrialEnds     *string
	orgCompedUntil   *string
	orgReason        *string
	orgExemptLimits  *string
	orgLiftUntil     *string
	orgExtra         *string
	orgBillingJSON   *bool
	orgEndLift       *bool
	orgClearFailOpen *bool
)

type billingCommand func(ctx context.Context, w io.Writer, svc billing.OperatorService, by billing.Actor, args []string) error

func billingRun(fn billingCommand) func(*cobra.Command, []string) {
	return func(_ *cobra.Command, args []string) {
		if err := runBilling(context.Background(), os.Stdout, fn, args); err != nil {
			log.Fatal(err)
		}
	}
}

// runBilling loads the config and the plans file, connects and runs fn with the operator billing service.
func runBilling(ctx context.Context, w io.Writer, fn billingCommand, args []string) error {
	mLog := logger.NewMemLogger()
	if err := loadCommandConfig(&mLog); err != nil {
		return fmt.Errorf("invalid config: %w. See rportd --help", err)
	}
	if !cfg.Storage.IsPostgres() || !cfg.Billing.Enabled {
		return errors.New(`billing needs [storage] backend = "postgres" and [billing] enabled`)
	}
	if rootRefused(cfg) {
		return errors.New(rootRefusedMsg)
	}
	plans, err := billing.LoadPlans(cfg.Billing.PlansFile)
	if err != nil {
		return err
	}
	db, err := pgserver.OpenOwner(ctx, cfg, *orgDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	by := cliActor()
	return fn(ctx, w, &pgbilling.Operator{DB: db, Plans: plans}, billing.Actor{Username: by.Username}, args)
}

// parseWhen reads a date (YYYY-MM-DD, midnight UTC) or an RFC 3339 time.
func parseWhen(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil, fmt.Errorf("want a date (YYYY-MM-DD) or an RFC 3339 time, not %q", s)
	}
	return &t, nil
}

func formatDate(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format("2006-01-02")
}

func printAccount(w io.Writer, verb string, a *billing.Account) {
	fmt.Fprintf(w, "%s billing of %q: plan %s, status %s", verb, a.Organization, a.Plan, a.Status)
	switch a.Status {
	case billing.StatusTrial:
		fmt.Fprintf(w, " until %s", formatDate(a.TrialEndsAt))
	case billing.StatusComped:
		fmt.Fprintf(w, " until %s", formatDate(a.CompedUntil))
	case billing.StatusExempt:
		fmt.Fprintf(w, " (limits: %s)", a.ExemptLimits)
	}
	if a.Reason != "" {
		fmt.Fprintf(w, ", reason %q", a.Reason)
	}
	fmt.Fprintf(w, ", transfer %s of %s (%s)\n", billing.ByteSize(a.Policy.UsedBytes), billing.ByteSize(a.Policy.AllowanceBytes), a.TransferState)
}

func runOrgSetPlan(ctx context.Context, w io.Writer, svc billing.OperatorService, by billing.Actor, args []string) error {
	a, err := svc.SetPlan(ctx, by, args[0], args[1])
	if err != nil {
		return err
	}
	printAccount(w, "updated", a)
	return nil
}

func runOrgSetBilling(ctx context.Context, w io.Writer, svc billing.OperatorService, by billing.Actor, args []string) error {
	c := billing.StatusChange{Status: billing.Status(strings.ToLower(args[1])), Reason: *orgReason, ExemptLimits: billing.ExemptLimits(*orgExemptLimits)}
	var err error
	if c.TrialEndsAt, err = parseWhen(*orgTrialEnds); err != nil {
		return fmt.Errorf("--trial-ends: %w", err)
	}
	if c.CompedUntil, err = parseWhen(*orgCompedUntil); err != nil {
		return fmt.Errorf("--comped-until: %w", err)
	}
	a, err := svc.SetStatus(ctx, by, args[0], c)
	if err != nil {
		return err
	}
	printAccount(w, "updated", a)
	return nil
}

func runOrgLiftCap(ctx context.Context, w io.Writer, svc billing.OperatorService, by billing.Actor, args []string) error {
	until, err := parseWhen(*orgLiftUntil)
	if err != nil {
		return fmt.Errorf("--until: %w", err)
	}
	if until == nil && *orgExtra == "" && !*orgEndLift && !*orgClearFailOpen {
		return errors.New("give --until (lift the hard cap until then), --end, --extra-transfer (raise this month's allowance) or --clear-fail-open")
	}
	a, err := svc.SetOverride(ctx, by, args[0], billing.Override{CapLiftedUntil: until, EndCapLift: *orgEndLift,
		ExtraTransfer: *orgExtra, ClearFailOpen: *orgClearFailOpen})
	if err != nil {
		return err
	}
	printAccount(w, "updated", a)
	return nil
}

func runOrgApprovePeriod(ctx context.Context, w io.Writer, svc billing.OperatorService, by billing.Actor, args []string) error {
	month, err := time.Parse("2006-01", args[1])
	if err != nil {
		return fmt.Errorf("the period is a month, YYYY-MM, not %q", args[1])
	}
	if err := svc.ApprovePeriod(ctx, by, args[0], month); err != nil {
		return err
	}
	fmt.Fprintf(w, "approved the period %s of %q: it is charged\n", args[1], args[0])
	return nil
}

func runOrgBilling(ctx context.Context, w io.Writer, svc billing.OperatorService, _ billing.Actor, args []string) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if len(args) == 1 {
		a, err := svc.Account(ctx, args[0])
		if err != nil {
			return err
		}
		ps, err := svc.Periods(ctx, args[0], "")
		if err != nil {
			return err
		}
		if *orgBillingJSON {
			return enc.Encode(map[string]any{"account": a, "periods": ps})
		}
		printAccount(w, "current", a)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PERIOD\tPLAN\tSTATUS\tDEVICES\tTRANSFER\tALLOWANCE\tOVERAGE GIB\tTOTAL USD\tSTATE")
		for _, p := range ps {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%d\t%.2f\t%s\n", p.Start.UTC().Format("2006-01"), p.Plan, p.Status, p.BillableDevices,
				billing.ByteSize(p.TransferBytes), billing.ByteSize(p.AllowanceBytes), p.OverageGiB, float64(p.TotalCents)/100, p.State)
		}
		return tw.Flush()
	}
	all, err := svc.Accounts(ctx)
	if err != nil {
		return err
	}
	if *orgBillingJSON {
		return enc.Encode(all)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPLAN\tSTATUS\tUNTIL\tTRANSFER\tALLOWANCE\tSTATE")
	for _, a := range all {
		until := "-"
		switch a.Status {
		case billing.StatusTrial:
			until = formatDate(a.TrialEndsAt)
		case billing.StatusComped:
			until = formatDate(a.CompedUntil)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", a.Organization, a.Plan, a.Status, until,
			billing.ByteSize(a.Policy.UsedBytes), billing.ByteSize(a.Policy.AllowanceBytes), a.TransferState)
	}
	return tw.Flush()
}

func init() {
	orgCmd.AddCommand(orgSetPlanCmd, orgSetBillingCmd, orgBillingCmd, orgLiftCapCmd, orgApprovePeriodCmd)
	orgTrialEnds = orgSetBillingCmd.Flags().String("trial-ends", "", "when a trial ends (date or RFC 3339 time)")
	orgCompedUntil = orgSetBillingCmd.Flags().String("comped-until", "", "the last day (UTC) a comped organization is not charged")
	orgReason = orgSetBillingCmd.Flags().String("reason", "", "why (required for exempt)")
	orgExemptLimits = orgSetBillingCmd.Flags().String("exempt-limits", "", `limits of an exempt organization: "plan" (default) or "none"`)
	orgLiftUntil = orgLiftCapCmd.Flags().String("until", "", "lift the hard cap until then (date or RFC 3339 time)")
	orgExtra = orgLiftCapCmd.Flags().String("extra-transfer", "", "raise this month's transfer allowance, e.g. 50GiB")
	orgBillingJSON = orgBillingCmd.Flags().Bool("json", false, "print JSON")
	orgEndLift = orgLiftCapCmd.Flags().Bool("end", false, "end a cap lift")
	orgClearFailOpen = orgLiftCapCmd.Flags().Bool("clear-fail-open", false, "let the hard cap apply again in a month that failed open")
}

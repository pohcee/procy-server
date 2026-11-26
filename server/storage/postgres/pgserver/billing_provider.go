package pgserver

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/server/billing"
	"github.com/pohcee/procy-server/server/billing/stripe"
	"github.com/pohcee/procy-server/server/chconfig"
	"github.com/pohcee/procy-server/server/operator"
	pgbilling "github.com/pohcee/procy-server/server/storage/postgres/billing"
	"github.com/pohcee/procy-server/server/storage/postgres/billingprovider"
	"github.com/pohcee/procy-server/share/logger"
)

// BillingProvider returns the payment provider service of [billing] provider on db (nil for "none"): Stripe with
// its keys read from their files, the provider's store (migration 1500), the billing records and closed periods of
// migration 1450, and suspensions through ops.
func BillingProvider(cfg *chconfig.Config, db *sqlx.DB, plans *billing.Plans, ops operator.Service) (*billing.Service, error) {
	if cfg.Billing.Provider != chconfig.BillingProviderStripe {
		return nil, nil
	}
	key, err := readSecret(cfg.Billing.StripeSecretKeyFile)
	if err != nil {
		return nil, fmt.Errorf("[billing] stripe_secret_key_file: %w", err)
	}
	whsec, err := readSecret(cfg.Billing.StripeWebhookSecretFile)
	if err != nil {
		return nil, fmt.Errorf("[billing] stripe_webhook_secret_file: %w", err)
	}
	p, err := stripe.New(stripe.Options{SecretKey: key, WebhookSecret: whsec, AutomaticTax: cfg.Billing.AutomaticTax()})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(plans.List))
	for _, pl := range plans.List {
		ids = append(ids, pl.ID)
	}
	store := pgbilling.Store{DB: db, Plans: plans}
	var susp billing.Suspender
	if ops != nil {
		susp = suspender{ops: ops}
	}
	provStore := billingprovider.Store{DB: db}
	return billing.NewService(billing.Options{
		Provider: p,
		Store:    provStore,
		// webhook event ids are kept far longer than Stripe retries (3 days)
		PruneEvents: func(ctx context.Context) error { return provStore.PruneEvents(ctx, 90*24*time.Hour) },
		Plans:       plans,
		PlanIDs:     ids,
		FreePlan:    plans.Free,
		Records:     billingprovider.Records{Store: store},
		Periods:     billingprovider.Periods{Operator: &pgbilling.Operator{DB: db, Plans: plans}},
		Sync:        billingprovider.Sync{DB: db, FreePlan: plans.Free, Plans: plans, By: "billing:" + p.Name()},
		Suspender:   susp,
		Dunning: billing.DunningPolicy{
			Grace:    time.Duration(cfg.Billing.GraceDays) * 24 * time.Hour,
			ReadOnly: time.Duration(cfg.Billing.ReadOnlyDays) * 24 * time.Hour,
		},
		ReturnURL: cfg.Billing.ReturnURL,
		Log:       logger.NewLogger("billing-provider", cfg.Logging.LogOutput, cfg.Logging.LogLevel),
	})
}

// readSecret reads a secret file: its content without surrounding whitespace, which must not be empty. The
// secret is never logged.
func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}

// suspender suspends organizations through the operator service (audited in the system tenant).
type suspender struct{ ops operator.Service }

func (s suspender) Suspend(ctx context.Context, by billing.Actor, org string) error {
	_, err := s.ops.SetStatus(ctx, operator.Actor{Username: by.Username, RemoteIP: by.RemoteIP}, org, operator.StatusSuspended)
	return err
}

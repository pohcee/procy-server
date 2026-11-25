package billing_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/billing"
	"github.com/openrport/openrport/server/billing/stripe/stripetest"
)

func TestCancelForDeletion(t *testing.T) {
	e := newEnv(t)
	e.fake.Subscriptions["sub_acme"] = &stripetest.Subscription{ID: "sub_acme", Customer: "cus_1", Status: "active"}
	e.records["acme"] = &billing.Billing{Organization: "acme", Status: billing.StatusPaying, ProviderSubscriptionID: "sub_acme"}
	require.NoError(t, e.svc.CancelForDeletion(ctx, "acme"))
	assert.Equal(t, "canceled", e.fake.Subscriptions["sub_acme"].Status, "canceled at once")
	require.NoError(t, e.svc.CancelForDeletion(ctx, "nosub"), "nothing to cancel")
}

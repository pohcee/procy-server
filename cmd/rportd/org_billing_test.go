package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/billing"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
)

const testPlans = `
free_plan = "free"
[[plan]]
  id = "free"
  max_clients = 10
  transfer_base = "10GiB"
  overage = "cap"
[[plan]]
  id = "team"
  name = "Team"
  base_price = 49.0
  transfer_base = "250GiB"
  overage = "bill"
  overage_price_per_gib = 0.2
`

func writeBillingConfig(t *testing.T, dsn string) {
	t.Helper()
	plans := filepath.Join(t.TempDir(), "plans.toml")
	require.NoError(t, os.WriteFile(plans, []byte(testPlans), 0o600))
	keyFile := filepath.Join(t.TempDir(), "vault.key")
	require.NoError(t, os.WriteFile(keyFile, []byte(strings.Repeat("ab", 32)+"\n"), 0o600))
	writeConfig(t, "[server]\n  data_dir = \""+t.TempDir()+"\"\n[storage]\n  backend = \"postgres\"\n  dsn = \""+dsn+"\"\n"+
		"[vault]\n  key_provider = \"local\"\n  local_key_file = \""+strings.ReplaceAll(keyFile, `\`, `\\`)+"\"\n"+
		"[metering]\n  enabled = true\n[billing]\n  enabled = true\n  plans_file = \""+strings.ReplaceAll(plans, `\`, `\\`)+"\"\n")
}

func bill(t *testing.T, fn billingCommand, args ...string) (string, error) {
	t.Helper()
	var b bytes.Buffer
	err := runBilling(context.Background(), &b, fn, args)
	return b.String(), err
}

func TestOrgBillingCommands(t *testing.T) {
	d := pgtest.New(t, "acme")
	writeBillingConfig(t, d.AppURL)

	out, err := bill(t, runOrgSetPlan, "acme", "team")
	require.NoError(t, err)
	assert.Contains(t, out, `updated billing of "acme": plan team, status paying`)
	_, err = bill(t, runOrgSetPlan, "acme", "gold")
	assert.ErrorIs(t, err, billing.ErrInvalid)

	setFlag(t, orgReason, "partner")
	out, err = bill(t, runOrgSetBilling, "acme", "exempt")
	require.NoError(t, err)
	assert.Contains(t, out, `status exempt (limits: plan), reason "partner"`)
	*orgReason = ""
	setFlag(t, orgTrialEnds, "2099-01-31")
	out, err = bill(t, runOrgSetBilling, "acme", "trial")
	require.NoError(t, err)
	assert.Contains(t, out, "status trial until 2099-01-31")
	*orgTrialEnds = ""
	_, err = bill(t, runOrgSetBilling, "acme", "comped")
	assert.ErrorIs(t, err, billing.ErrInvalid)

	setFlag(t, orgExtra, "5GiB")
	_, err = bill(t, runOrgLiftCap, "acme")
	require.NoError(t, err)
	_, err = bill(t, runOrgApprovePeriod, "acme", "2026-10")
	assert.ErrorIs(t, err, billing.ErrNotFound)

	out, err = bill(t, runOrgBilling)
	require.NoError(t, err)
	assert.Regexp(t, `(?m)^acme\s+team\s+trial\s+2099-01-31\s+`, out)
	out, err = bill(t, runOrgBilling, "acme")
	require.NoError(t, err)
	assert.Contains(t, out, "PERIOD")

	var actions []string
	require.NoError(t, d.Owner.Select(&actions, "SELECT action FROM auditlog WHERE tenant_id = '_system' AND application = 'billing' AND username LIKE 'cli:%' ORDER BY id"))
	assert.Equal(t, []string{"set_plan", "set_status", "set_status", "override"}, actions)
}

func TestOrgBillingNeedsBilling(t *testing.T) {
	d := pgtest.New(t)
	writePostgresConfig(t, d.AppURL)
	_, err := bill(t, runOrgSetPlan, "acme", "team")
	assert.ErrorContains(t, err, "[billing] enabled")
}

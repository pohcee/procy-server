package clientgroups

import (
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/cgroups"
	"github.com/openrport/openrport/server/repotest"

	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

func TestClientGroupsContract(t *testing.T) {
	repotest.ClientGroups(t, func(t *testing.T) cgroups.ClientGroupProvider {
		d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
		return New(d.App)
	})
}

func group(id, desc string) *cgroups.ClientGroup {
	return &cgroups.ClientGroup{ID: id, Description: desc, Params: &cgroups.ClientParams{}, AllowedUserGroups: []string{}}
}

func setup(t *testing.T) (*pgtest.DB, *Store) {
	d := pgtest.New(t, repotest.TenantA, repotest.TenantB)
	s := New(d.App)
	require.NoError(t, s.Create(repotest.Ctx(repotest.TenantA), group("g1", "a")))
	require.NoError(t, s.Create(repotest.Ctx(repotest.TenantB), group("g1", "b")))
	return d, s
}

func TestRLSHidesOtherTenantsGroups(t *testing.T) {
	d, _ := setup(t)
	var descs []string
	require.NoError(t, postgres.InTenant(repotest.Ctx(repotest.TenantA), d.App, func(tx *sqlx.Tx) error {
		return tx.Select(&descs, "SELECT description FROM client_groups")
	}))
	assert.Equal(t, []string{"a"}, descs)

	var n int
	require.NoError(t, d.App.Get(&n, "SELECT count(*) FROM client_groups"))
	assert.Zero(t, n)
}

func TestDeletingTenantDeletesGroups(t *testing.T) {
	d, _ := setup(t)
	_, err := d.Owner.Exec("DELETE FROM tenants WHERE id = 'a'")
	require.NoError(t, err)
	var descs []string
	require.NoError(t, d.Owner.Select(&descs, "SELECT description FROM client_groups"))
	assert.Equal(t, []string{"b"}, descs)
}

// Columns hold the JSON the OSS types write, so rows copied from SQLite read back unchanged.
func TestRowsAsSQLiteWrites(t *testing.T) {
	d, s := setup(t)
	_, err := d.Owner.Exec(`INSERT INTO client_groups (tenant_id, id, description, params, allowed_user_groups)
		VALUES ('a', 'copied', 'copied', '{"os":["linux*"],"tag":{"or":["x"]}}', '["ops"]')`)
	require.NoError(t, err)
	got, err := s.Get(repotest.Ctx(repotest.TenantA), "copied")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, &cgroups.ParamValues{"linux*"}, got.Params.OS)
	assert.JSONEq(t, `{"or":["x"]}`, string(*got.Params.Tag))
	assert.Equal(t, []string{"ops"}, []string(got.AllowedUserGroups))

	var params, allowed string
	require.NoError(t, d.Owner.QueryRow("SELECT params, allowed_user_groups FROM client_groups WHERE tenant_id = 'a' AND id = 'g1'").Scan(&params, &allowed))
	want, err := (&cgroups.ClientParams{}).Value()
	require.NoError(t, err)
	assert.Equal(t, want, params)
	assert.Equal(t, "[]", allowed)
}

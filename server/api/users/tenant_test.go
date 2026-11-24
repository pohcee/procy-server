package users

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	chshare "github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/tenant"
)

func ctxFor(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

var defaultCtx = ctxFor(tenant.Default)

// assertProviderIsDefaultOnly checks that every storage-reaching method of a single-tenant provider refuses a
// missing tenant with ErrMissing and any other tenant with ErrNotSupported.
func assertProviderIsDefaultOnly(t *testing.T, p Provider) {
	t.Helper()
	for _, tc := range []struct {
		ctx  context.Context
		want error
	}{
		{context.Background(), tenant.ErrMissing},
		{ctxFor("other"), tenant.ErrNotSupported},
	} {
		ctx := tc.ctx
		_, err := p.GetAll(ctx)
		assert.ErrorIs(t, err, tc.want, "GetAll")
		_, err = p.GetByUsername(ctx, "user1")
		assert.ErrorIs(t, err, tc.want, "GetByUsername")
		_, err = p.ListGroups(ctx)
		assert.ErrorIs(t, err, tc.want, "ListGroups")
		_, err = p.GetGroup(ctx, "group1")
		assert.ErrorIs(t, err, tc.want, "GetGroup")
		assert.ErrorIs(t, p.UpdateGroup(ctx, "group1", Group{Name: "group1"}), tc.want, "UpdateGroup")
		assert.ErrorIs(t, p.DeleteGroup(ctx, "group1"), tc.want, "DeleteGroup")
		assert.ErrorIs(t, p.Add(ctx, &User{Username: "new", Password: "x"}), tc.want, "Add")
		assert.ErrorIs(t, p.Update(ctx, &User{Password: "x"}, "user1"), tc.want, "Update")
		assert.ErrorIs(t, p.Delete(ctx, "user1"), tc.want, "Delete")
	}
}

func TestStaticProviderIsDefaultOnly(t *testing.T) {
	p := NewStaticProvider([]*User{{Username: "user1", Password: "pass1"}})
	assertProviderIsDefaultOnly(t, p)

	u, err := p.GetByUsername(defaultCtx, "user1")
	require.NoError(t, err)
	require.NotNil(t, u)
	assert.Equal(t, "pass1", u.Password)
}

func TestFileAdapterIsDefaultOnly(t *testing.T) {
	logger := chshare.NewLogger("file-adapter-test", chshare.LogOutput{File: os.Stdout}, chshare.LogLevelDebug)
	fm := &FileManagerMock{Users: []*User{{Username: "user1", Password: "pass1"}}}

	_, err := NewFileAdapter(context.Background(), logger, fm)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = NewFileAdapter(ctxFor("other"), logger, fm)
	assert.ErrorIs(t, err, tenant.ErrNotSupported)

	fa, err := NewFileAdapter(defaultCtx, logger, fm)
	require.NoError(t, err)
	assertProviderIsDefaultOnly(t, fa)
	// the refused writes didn't touch the file
	require.Len(t, fm.Users, 1)
	assert.Equal(t, "pass1", fm.Users[0].Password)

	u, err := fa.GetByUsername(defaultCtx, "user1")
	require.NoError(t, err)
	require.NotNil(t, u)
}

func TestUserDatabaseIsDefaultOnly(t *testing.T) {
	db, err := sqlx.Connect("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close()
	require.NoError(t, prepareTables(db, false, false, false))
	require.NoError(t, prepareDummyData(db, false, false, false))
	var before int
	require.NoError(t, db.Get(&before, "SELECT COUNT(*) FROM users"))

	d, err := NewUserDatabase(db, "users", "groups", "group_details", false, false, false, testLog)
	require.NoError(t, err)
	assertProviderIsDefaultOnly(t, d)

	var after int
	require.NoError(t, db.Get(&after, "SELECT COUNT(*) FROM users"))
	assert.Equal(t, before, after)

	u, err := d.GetByUsername(defaultCtx, "user1")
	require.NoError(t, err)
	require.NotNil(t, u)
}

func TestUserCacheIsolatesTenants(t *testing.T) {
	c := NewUserCache(nil)
	require.NoError(t, c.Load(ctxFor("a"), []*User{{Username: "u", Password: "from a"}}))
	require.NoError(t, c.Load(ctxFor("b"), []*User{{Username: "u", Password: "from b"}, {Username: "only-b"}}))

	u, err := c.GetByUsername(ctxFor("a"), "u")
	require.NoError(t, err)
	assert.Equal(t, "from a", u.Password)
	u, err = c.GetByUsername(ctxFor("a"), "only-b")
	require.NoError(t, err)
	assert.Nil(t, u)

	all, err := c.GetAll(ctxFor("b"))
	require.NoError(t, err)
	assert.Len(t, all, 2)

	// reloading a doesn't drop b
	require.NoError(t, c.Load(ctxFor("a"), nil))
	all, err = c.GetAll(ctxFor("b"))
	require.NoError(t, err)
	assert.Len(t, all, 2)

	assert.ErrorIs(t, c.Load(context.Background(), nil), tenant.ErrMissing)
	_, err = c.GetByUsername(context.Background(), "u")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = c.GetAll(context.Background())
	assert.ErrorIs(t, err, tenant.ErrMissing)
}

func TestFileAdapterReloadUsesItsContext(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reloadCtx  context.Context
		wantReload bool
	}{
		{"server context", defaultCtx, true},
		{"tenant-less context", context.Background(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logger := chshare.NewLogger("file-adapter-test", chshare.LogOutput{File: os.Stdout}, chshare.LogLevelDebug)
			fm := &FileManagerMock{Users: []*User{{Username: "user1", Password: "pass1"}}}
			fa, err := NewFileAdapter(defaultCtx, logger, fm)
			require.NoError(t, err)

			fm.Users = []*User{{Username: "user1", Password: "pass1"}, {Username: "user2", Password: "pass2"}}
			signals := make(chan os.Signal)
			ctx, cancel := context.WithCancel(tc.reloadCtx)
			done := make(chan struct{})
			go func() { fa.reloadOn(ctx, signals); close(done) }()
			signals <- os.Interrupt
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("reload loop didn't stop when its context was cancelled")
			}

			u, err := fa.GetByUsername(defaultCtx, "user2")
			require.NoError(t, err)
			assert.Equal(t, tc.wantReload, u != nil)
		})
	}
}

func TestAPIServiceRequiresTenant(t *testing.T) {
	s := NewAPIService(NewStaticProvider([]*User{{Username: "user1", Password: "pass1"}}), false, 0, -1)
	ctx := context.Background()
	_, err := s.GetAll(ctx)
	assert.ErrorIs(t, err, tenant.ErrMissing)
	_, err = s.GetByUsername(ctx, "user1")
	assert.ErrorIs(t, err, tenant.ErrMissing)
	assert.ErrorIs(t, s.Delete(ctx, "user1"), tenant.ErrMissing)

	u, err := s.GetByUsername(defaultCtx, "user1")
	require.NoError(t, err)
	assert.NotNil(t, u)
}

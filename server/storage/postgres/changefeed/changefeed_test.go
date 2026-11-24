package changefeed_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/changefeed"
	"github.com/openrport/openrport/server/storage/postgres/migrate"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	"github.com/openrport/openrport/share/tenant"
)

func TestMain(m *testing.M) { pgtest.Main(m) }

type got struct{ Tenant, Entity, Key, Op string }

func simplify(cs []changefeed.Change) []got {
	out := []got{}
	for _, c := range cs {
		out = append(out, got{string(c.Tenant), c.Entity, c.Key, c.Op})
	}
	return out
}

func exec(t *testing.T, db *sqlx.DB, tid tenant.ID, q string, args ...any) {
	t.Helper()
	require.NoError(t, postgres.InTenantID(context.Background(), db, tid, func(tx *sqlx.Tx) error {
		_, err := tx.Exec(q, args...)
		return err
	}))
}

// readUntil reads until at least n changes after after were consumed (or 10s passed): on a Postgres server shared
// with other tests, a transaction elsewhere can hold rows back for a moment.
func readUntil(t *testing.T, db *sqlx.DB, after changefeed.Cursor, n int) ([]changefeed.Change, changefeed.Cursor) {
	t.Helper()
	var all []changefeed.Change
	deadline := time.Now().Add(10 * time.Second)
	for {
		cs, next := readAll(t, db, after)
		all, after = append(all, cs...), next
		if len(all) >= n || time.Now().After(deadline) {
			return all, after
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readAll(t *testing.T, db *sqlx.DB, after changefeed.Cursor) ([]changefeed.Change, changefeed.Cursor) {
	t.Helper()
	cs, next, err := changefeed.Read(context.Background(), db, after, 1000)
	require.NoError(t, err)
	return cs, next
}

func TestFeedRecordsChangesOfTrackedTables(t *testing.T) {
	d := pgtest.New(t, "a", "b")
	ctx := context.Background()
	head, err := changefeed.Head(ctx, d.App)
	require.NoError(t, err)

	exec(t, d.App, "a", "INSERT INTO users (tenant_id, username, password) VALUES ('a', 'alice', 'x')")
	exec(t, d.App, "b", "INSERT INTO clients_auth (tenant_id, id, password) VALUES ('b', 'c1', 'p')")
	exec(t, d.App, "a", "UPDATE users SET password = 'y' WHERE tenant_id = 'a' AND username = 'alice'")
	exec(t, d.App, "a", "UPDATE users SET username = 'alicia' WHERE tenant_id = 'a' AND username = 'alice'")
	exec(t, d.App, "a", "DELETE FROM users WHERE tenant_id = 'a'")
	// not tracked: no feed row
	exec(t, d.App, "a", "INSERT INTO library_meta (tenant_id, key, value) VALUES ('a', 'k', 'v')")

	cs, next := readUntil(t, d.App, head, 6)
	assert.Equal(t, []got{
		{"a", "users", "alice", "insert"},
		{"b", "clients_auth", "c1", "insert"},
		{"a", "users", "alice", "update"},
		{"a", "users", "alice", "delete"},
		{"a", "users", "alicia", "insert"},
		{"a", "users", "alicia", "delete"},
	}, simplify(cs))

	cs, again := readAll(t, d.App, next)
	assert.Empty(t, cs, "nothing after the cursor")
	assert.Equal(t, next, again)
}

func TestFeedTracksEveryCachedTable(t *testing.T) {
	d := pgtest.New(t)
	var tables []string
	require.NoError(t, d.Owner.Select(&tables, `SELECT c.relname FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
		WHERE t.tgname = 'record_change' ORDER BY 1`))
	assert.Equal(t, []string{"api_sessions", "api_tokens", "client_groups", "client_state", "clients", "clients_auth", "groups",
		"library_entries", "schedules", "stored_tunnels", "user_groups", "users", "vault_wrapped_keys"}, tables)
}

func TestFeedAppRoleCannotWriteIt(t *testing.T) {
	d := pgtest.New(t, "a")
	err := postgres.InTenantID(context.Background(), d.App, "a", func(tx *sqlx.Tx) error {
		_, err := tx.Exec("INSERT INTO changes (tenant_id, entity, key, op) VALUES ('a', 'users', 'x', 'insert')")
		return err
	})
	assert.ErrorContains(t, err, "permission denied")
	_, err = d.App.Exec("UPDATE changes_horizon SET seq = 1")
	assert.ErrorContains(t, err, "permission denied")
}

func TestFeedTenantDeletionIsRecorded(t *testing.T) {
	d := pgtest.New(t, "a")
	ctx := context.Background()
	exec(t, d.App, "a", "INSERT INTO users (tenant_id, username, password) VALUES ('a', 'alice', 'x')")
	head, err := changefeed.Head(ctx, d.App)
	require.NoError(t, err)
	_, err = d.Owner.Exec("DELETE FROM tenants WHERE id = 'a'")
	require.NoError(t, err, "the owner's cascading delete writes feed rows")
	cs, _ := readUntil(t, d.App, head, 1)
	assert.Contains(t, simplify(cs), got{"a", "users", "alice", "delete"})
}

// begin starts a transaction in tenant a on its own connection, for interleaving two writers.
func begin(t *testing.T, db *sqlx.DB) *sqlx.Tx {
	t.Helper()
	tx, err := db.Beginx()
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	_, err = tx.Exec("SELECT set_config('rport.tenant_id', 'a', true)")
	require.NoError(t, err)
	return tx
}

func TestFeedLateCommitIsNotSkipped(t *testing.T) {
	d := pgtest.New(t, "a")
	ctx := context.Background()
	head, err := changefeed.Head(ctx, d.App)
	require.NoError(t, err)

	// early takes its transaction id first but writes its feed row after late wrote its own: early has the lower
	// transaction id and the higher seq. early commits first, late last.
	early := begin(t, d.App)
	_, err = early.Exec("SELECT pg_current_xact_id()") // the oldest transaction id of the two
	require.NoError(t, err)
	late := begin(t, d.App)
	_, err = late.Exec("INSERT INTO users (tenant_id, username) VALUES ('a', 'late')") // lower seq, higher tx id
	require.NoError(t, err)
	_, err = early.Exec("INSERT INTO users (tenant_id, username) VALUES ('a', 'early')") // higher seq, lower tx id
	require.NoError(t, err)

	cs, cur := readAll(t, d.App, head)
	assert.Empty(t, cs, "both transactions running: nothing to consume")

	require.NoError(t, early.Commit())
	cs, cur = readUntil(t, d.App, cur, 1)
	assert.Equal(t, []got{{"a", "users", "early", "insert"}}, simplify(cs), "early is older than every running transaction")

	// late's row has a lower seq than the row just consumed: a seq cursor would skip it
	var lateSeq, earlySeq int64
	require.NoError(t, d.Owner.Get(&earlySeq, "SELECT seq FROM changes WHERE key = 'early'"))
	_, err = late.Exec("SELECT 1")
	require.NoError(t, err)
	require.NoError(t, late.Commit())
	require.NoError(t, d.Owner.Get(&lateSeq, "SELECT seq FROM changes WHERE key = 'late'"))
	require.Less(t, lateSeq, earlySeq, "the case under test: the late commit has the lower seq")

	cs, _ = readUntil(t, d.App, cur, 1)
	assert.Equal(t, []got{{"a", "users", "late", "insert"}}, simplify(cs), "the late commit is consumed after all")
}

func TestFeedRunningTransactionHoldsBackLaterCommits(t *testing.T) {
	d := pgtest.New(t, "a")
	ctx := context.Background()
	head, err := changefeed.Head(ctx, d.App)
	require.NoError(t, err)

	first := begin(t, d.App)
	_, err = first.Exec("INSERT INTO users (tenant_id, username) VALUES ('a', 'first')")
	require.NoError(t, err)
	exec(t, d.App, "a", "INSERT INTO users (tenant_id, username) VALUES ('a', 'second')")

	cs, cur := readAll(t, d.App, head)
	assert.Empty(t, cs, "second committed, but an older transaction is still running")
	require.NoError(t, first.Commit())
	cs, _ = readUntil(t, d.App, cur, 2)
	assert.Equal(t, []got{{"a", "users", "first", "insert"}, {"a", "users", "second", "insert"}}, simplify(cs))
}

func TestFeedRolledBackWritesLeaveNoRows(t *testing.T) {
	d := pgtest.New(t, "a")
	head, err := changefeed.Head(context.Background(), d.App)
	require.NoError(t, err)
	tx := begin(t, d.App)
	_, err = tx.Exec("INSERT INTO users (tenant_id, username) VALUES ('a', 'gone')")
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	exec(t, d.App, "a", "INSERT INTO users (tenant_id, username) VALUES ('a', 'kept')")
	cs, _ := readUntil(t, d.App, head, 1)
	assert.Equal(t, []got{{"a", "users", "kept", "insert"}}, simplify(cs))
}

func TestFeedRetention(t *testing.T) {
	d := pgtest.New(t, "a")
	ctx := context.Background()
	head, err := changefeed.Head(ctx, d.App)
	require.NoError(t, err)
	exec(t, d.App, "a", "INSERT INTO users (tenant_id, username) VALUES ('a', 'u1')")
	exec(t, d.App, "a", "INSERT INTO users (tenant_id, username) VALUES ('a', 'u2')")

	n, err := changefeed.Prune(ctx, d.App, time.Hour)
	require.NoError(t, err)
	assert.Zero(t, n, "nothing is an hour old")
	cs, caughtUp := readUntil(t, d.App, head, 2)
	assert.Len(t, cs, 2)

	// prune removes only rows of transactions older than every running one: retry while one elsewhere holds them
	var removed int64
	require.Eventually(t, func() bool {
		n, err := changefeed.Prune(ctx, d.App, 0)
		require.NoError(t, err)
		removed += n
		return removed >= 2
	}, 10*time.Second, 20*time.Millisecond)
	assert.EqualValues(t, 2, removed)

	_, _, err = changefeed.Read(ctx, d.App, head, 10)
	assert.ErrorIs(t, err, changefeed.ErrTooFarBehind, "a reader before the pruned rows missed them")
	cs, _, err = changefeed.Read(ctx, d.App, caughtUp, 10)
	assert.NoError(t, err, "a reader that had consumed them is not behind")
	assert.Empty(t, cs)

	fresh, err := changefeed.Head(ctx, d.App)
	require.NoError(t, err)
	assert.False(t, fresh.Less(caughtUp), "the head of an empty feed is the horizon")
	exec(t, d.App, "a", "INSERT INTO users (tenant_id, username) VALUES ('a', 'u3')")
	cs, _ = readUntil(t, d.App, fresh, 1)
	assert.Equal(t, []got{{"a", "users", "u3", "insert"}}, simplify(cs))
}

type recorder struct {
	mu      sync.Mutex
	changes []changefeed.Change
	resets  int
}

func (r *recorder) onChanges(_ context.Context, cs []changefeed.Change) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.changes = append(r.changes, cs...)
}

func (r *recorder) onReset(context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resets++
}

func (r *recorder) keys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.changes {
		out = append(out, c.Key)
	}
	return out
}

func follow(t *testing.T, f *changefeed.Follower) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = f.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case <-f.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("follower not ready")
	}
}

func TestFollowerWakesOnNotify(t *testing.T) {
	d := pgtest.New(t, "a")
	r := &recorder{}
	f := &changefeed.Follower{DB: d.App, OnChanges: r.onChanges, OnReset: r.onReset, PollInterval: time.Hour}
	follow(t, f)
	// the LISTEN connection is set up concurrently: write until a notification gets through (polling never fires)
	i := 0
	require.Eventually(t, func() bool {
		i++
		exec(t, d.App, "a", "INSERT INTO users (tenant_id, username) VALUES ('a', $1)", fmt.Sprintf("u%d", i))
		return len(r.keys()) > 0
	}, 10*time.Second, 100*time.Millisecond)
}

func TestFollowerPollsAndResets(t *testing.T) {
	d := pgtest.New(t, "a")
	r := &recorder{}
	f := &changefeed.Follower{DB: d.App, OnChanges: r.onChanges, OnReset: r.onReset, PollInterval: 50 * time.Millisecond}
	follow(t, f)
	exec(t, d.App, "a", "INSERT INTO users (tenant_id, username) VALUES ('a', 'polled')")
	require.Eventually(t, func() bool { return assert.ObjectsAreEqual([]string{"polled"}, r.keys()) }, 10*time.Second, 20*time.Millisecond)

}

func TestFollowerResetsWhenBehindRetention(t *testing.T) {
	d := pgtest.New(t, "a")
	ctx := context.Background()
	from, err := changefeed.Head(ctx, d.App)
	require.NoError(t, err)
	exec(t, d.App, "a", "INSERT INTO users (tenant_id, username) VALUES ('a', 'missed')")
	_, err = changefeed.Prune(ctx, d.App, 0)
	require.NoError(t, err)

	r := &recorder{}
	f := &changefeed.Follower{DB: d.App, From: &from, OnChanges: r.onChanges, OnReset: r.onReset, PollInterval: 50 * time.Millisecond}
	follow(t, f)
	require.Eventually(t, func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.resets == 1 }, 10*time.Second, 20*time.Millisecond)
	exec(t, d.App, "a", "INSERT INTO users (tenant_id, username) VALUES ('a', 'after')")
	require.Eventually(t, func() bool { return assert.ObjectsAreEqual([]string{"after"}, r.keys()) }, 10*time.Second, 20*time.Millisecond)
}

func TestFeedAheadOfDatabaseIsRefusedAndResetByMigrate(t *testing.T) {
	d := pgtest.New(t, "a")
	ctx := context.Background()
	require.NoError(t, changefeed.CheckNotAhead(ctx, d.App))
	// as after a restore into a server whose transaction ids are lower than the dumped ones
	_, err := d.Owner.Exec("UPDATE changes_horizon SET tx_id = '900000000000', seq = 7")
	require.NoError(t, err)
	assert.ErrorIs(t, changefeed.CheckNotAhead(ctx, d.App), changefeed.ErrAhead)

	_, err = migrate.Up(ctx, d.Owner)
	require.NoError(t, err)
	require.NoError(t, changefeed.CheckNotAhead(ctx, d.App), "rportd migrate resets the feed")
	head, err := changefeed.Head(ctx, d.App)
	require.NoError(t, err)
	exec(t, d.App, "a", "INSERT INTO users (tenant_id, username) VALUES ('a', 'after-reset')")
	cs, _ := readUntil(t, d.App, head, 1)
	assert.Equal(t, []got{{"a", "users", "after-reset", "insert"}}, simplify(cs))
}

func TestFollowerReportsStall(t *testing.T) {
	d := pgtest.New(t, "a")
	errs := make(chan error, 10)
	f := &changefeed.Follower{DB: d.App, PollInterval: 30 * time.Millisecond, StallWarning: 200 * time.Millisecond,
		OnError: func(err error) {
			select {
			case errs <- err:
			default:
			}
		}}
	follow(t, f)
	idle := begin(t, d.App)
	_, err := idle.Exec("INSERT INTO users (tenant_id, username) VALUES ('a', 'idle')")
	require.NoError(t, err)
	exec(t, d.App, "a", "INSERT INTO users (tenant_id, username) VALUES ('a', 'waiting')")
	select {
	case err := <-errs:
		assert.ErrorIs(t, err, changefeed.ErrStalled)
	case <-time.After(10 * time.Second):
		t.Fatal("no stall reported")
	}
	require.NoError(t, idle.Rollback())
}

func TestCursorString(t *testing.T) {
	c := changefeed.Cursor{TxID: 12345678901, Seq: 42}
	got, err := changefeed.ParseCursor(c.String())
	require.NoError(t, err)
	assert.Equal(t, c, got)
	_, err = changefeed.ParseCursor("42")
	assert.Error(t, err)
}

// applier stores its position with what it applied, and fails once on demand.
type applier struct {
	mu     sync.Mutex
	keys   []string
	pos    changefeed.Cursor
	failed bool
	fail   bool
	resets int
}

func (a *applier) Apply(_ context.Context, cs []changefeed.Change, next changefeed.Cursor) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail && !a.failed {
		a.failed = true
		return assert.AnError
	}
	for _, c := range cs {
		a.keys = append(a.keys, c.Key)
	}
	a.pos = next
	return nil
}

func (a *applier) Reset(_ context.Context, head changefeed.Cursor) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.resets++
	a.keys = nil
	a.pos = head
	return nil
}

func (a *applier) snapshot() ([]string, changefeed.Cursor) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.keys...), a.pos
}

func TestFollowerApplierResumesFromItsPosition(t *testing.T) {
	d := pgtest.New(t, "a")
	ctx := context.Background()
	start, err := changefeed.Head(ctx, d.App)
	require.NoError(t, err)
	exec(t, d.App, "a", "INSERT INTO users (tenant_id, username) VALUES ('a', 'one')")

	a := &applier{fail: true, pos: start}
	f := &changefeed.Follower{DB: d.App, From: &start, Applier: a, PollInterval: 30 * time.Millisecond}
	follow(t, f)
	require.Eventually(t, func() bool { k, _ := a.snapshot(); return len(k) == 1 }, 10*time.Second, 20*time.Millisecond)
	keys, pos := a.snapshot()
	assert.Equal(t, []string{"one"}, keys, "a failed apply is retried with the same changes, not skipped")

	// a second follower resuming from the stored position sees only what came after
	exec(t, d.App, "a", "INSERT INTO users (tenant_id, username) VALUES ('a', 'two')")
	b := &applier{pos: pos}
	follow(t, &changefeed.Follower{DB: d.App, From: &pos, Applier: b, PollInterval: 30 * time.Millisecond})
	require.Eventually(t, func() bool { k, _ := b.snapshot(); return len(k) == 1 }, 10*time.Second, 20*time.Millisecond)
	keys, _ = b.snapshot()
	assert.Equal(t, []string{"two"}, keys)
}

func TestMigrateRunsDeferredDDLOnceItsTableExists(t *testing.T) {
	d := pgtest.New(t, "a")
	ctx := context.Background()
	_, err := d.Owner.Exec(`INSERT INTO migrate_deferred (name, needs, ddl) VALUES ('9999 test', 'later_table',
		$d$SELECT rport_track_changes('later_table', 'later', 'id')$d$)`)
	require.NoError(t, err)
	assert.ErrorContains(t, migrate.Check(ctx, d.App), "9999 test is not applied", "a node refuses to start")

	_, err = migrate.Up(ctx, d.Owner)
	require.NoError(t, err)
	assert.Error(t, migrate.Check(ctx, d.App), "still pending: its table doesn't exist yet")

	_, err = d.Owner.Exec(`CREATE TABLE later_table (tenant_id text NOT NULL, id text NOT NULL);
		GRANT SELECT, INSERT ON later_table TO ` + pgtest.AppRole)
	require.NoError(t, err)
	_, err = migrate.Up(ctx, d.Owner)
	require.NoError(t, err)
	require.NoError(t, migrate.Check(ctx, d.App))

	head, err := changefeed.Head(ctx, d.App)
	require.NoError(t, err)
	exec(t, d.App, "a", "INSERT INTO later_table (tenant_id, id) VALUES ('a', 'x')")
	cs, _ := readUntil(t, d.App, head, 1)
	assert.Equal(t, []got{{"a", "later", "x", "insert"}}, simplify(cs))
}

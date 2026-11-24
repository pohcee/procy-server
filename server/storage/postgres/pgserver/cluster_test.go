package pgserver

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	chserver "github.com/openrport/openrport/server"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/server/library"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
	"github.com/openrport/openrport/share/files"
)

// feedDelay is long enough for a change committed on one node to reach the other (NOTIFY; polling every 3s backs
// it up).
const feedDelay = 1500 * time.Millisecond

// twoNodes runs nodes A and B (role api) on one database with the seeded tenants.
func twoNodes(t *testing.T) (*pgtest.DB, *nodeFixture, *nodeFixture) {
	t.Helper()
	d := pgtest.New(t)
	seedTenants(t, d)
	return d, startNodeOn(t, d, nodeSettings{nodeID: "node-a"}), startNodeOn(t, d, nodeSettings{nodeID: "node-b"})
}

func TestSessionRevokedOnOneNodeRefusedOnOther(t *testing.T) {
	_, a, b := twoNodes(t)
	tok := a.login(t, "alice")
	code, out := b.do(t, http.MethodGet, "/me", tok, nil)
	require.Equal(t, http.StatusOK, code, "B caches the session: %s", out["_raw"])

	code, out = a.do(t, http.MethodDelete, "/logout", tok, nil)
	require.Less(t, code, 300, out["_raw"])
	time.Sleep(feedDelay)
	// without the feed B would still accept this request from its cache (the cache entry goes only after a failed
	// lifetime update)
	code, out = b.do(t, http.MethodGet, "/me", tok, nil)
	assert.Equal(t, http.StatusUnauthorized, code, "the very first request on B after the revocation: %s", out["_raw"])
}

func TestUserDeletedOnOneNodeRefusedOnOther(t *testing.T) {
	_, a, b := twoNodes(t)
	alice := a.login(t, "alice")
	code, out := a.do(t, http.MethodPost, "/users", alice, map[string]any{"username": "carol", "password": testPassword, "groups": []string{"Administrators"}})
	require.Less(t, code, 300, out["_raw"])
	carol := b.login(t, "carol")
	code, out = b.do(t, http.MethodGet, "/me", carol, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])

	code, out = a.do(t, http.MethodDelete, "/users/carol", alice, nil)
	require.Less(t, code, 300, out["_raw"])
	time.Sleep(feedDelay)
	code, out = b.do(t, http.MethodGet, "/me", carol, nil)
	assert.Equal(t, http.StatusUnauthorized, code, out["_raw"])
}

func clientField(t *testing.T, h *nodeFixture, tok, field string) any {
	t.Helper()
	code, out := h.do(t, http.MethodGet, "/clients/web01", tok, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	return out["data"].(map[string]any)[field]
}

func TestClientACLChangedOnOneNodeSeenOnOther(t *testing.T) {
	_, a, b := twoNodes(t)
	aliceA, aliceB := a.login(t, "alice"), b.login(t, "alice")
	assert.Empty(t, clientField(t, b, aliceB, "allowed_user_groups"), "B has loaded the client")

	code, out := a.do(t, http.MethodPost, "/clients/web01/acl", aliceA, map[string]any{"allowed_user_groups": []string{"Administrators"}})
	require.Less(t, code, 300, out["_raw"])
	assert.Eventually(t, func() bool {
		return fmt.Sprint(clientField(t, b, aliceB, "allowed_user_groups")) == "[Administrators]"
	}, 10*time.Second, 100*time.Millisecond, "B's client cache follows A's change")

	// and the other way round
	code, out = b.do(t, http.MethodPost, "/clients/web01/acl", aliceB, map[string]any{"allowed_user_groups": []string{}})
	require.Less(t, code, 300, out["_raw"])
	assert.Eventually(t, func() bool {
		return fmt.Sprint(clientField(t, a, aliceA, "allowed_user_groups")) == "[]"
	}, 10*time.Second, 100*time.Millisecond)
}

func TestClientDeletedOnOneNodeGoneOnOther(t *testing.T) {
	_, a, b := twoNodes(t)
	aliceA, aliceB := a.login(t, "alice"), b.login(t, "alice")
	code, out := b.do(t, http.MethodGet, "/clients", aliceB, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	require.Equal(t, []string{"a-web"}, clientNames(t, out))

	code, out = a.do(t, http.MethodDelete, "/clients/web01", aliceA, nil)
	require.Less(t, code, 300, out["_raw"])
	assert.Eventually(t, func() bool {
		_, out := b.do(t, http.MethodGet, "/clients", aliceB, nil)
		return len(clientNames(t, out)) == 0
	}, 10*time.Second, 100*time.Millisecond)
}

func TestClientGroupAndClientAuthChangedOnOneNodeSeenOnOther(t *testing.T) {
	_, a, b := twoNodes(t)
	aliceA, aliceB := a.login(t, "alice"), b.login(t, "alice")
	code, out := a.do(t, http.MethodPost, "/client-groups", aliceA, map[string]any{"id": "g1", "description": "first", "params": map[string]any{"client_id": []string{"web*"}}})
	require.Less(t, code, 300, out["_raw"])
	code, out = b.do(t, http.MethodGet, "/client-groups/g1", aliceB, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Contains(t, out["_raw"], "web01", "the group's clients, from B's client cache")

	code, out = a.do(t, http.MethodPost, "/clients-auth", aliceA, map[string]any{"id": "new-device", "password": "a-long-device-password"})
	require.Less(t, code, 300, out["_raw"])
	code, out = b.do(t, http.MethodGet, "/clients-auth/new-device", aliceB, nil)
	assert.Equal(t, http.StatusOK, code, out["_raw"])
	code, out = a.do(t, http.MethodDelete, "/clients-auth/new-device", aliceA, nil)
	require.Less(t, code, 300, out["_raw"])
	code, out = b.do(t, http.MethodGet, "/clients-auth/new-device", aliceB, nil)
	assert.Equal(t, http.StatusNotFound, code, out["_raw"])
}

// scheduleRuns returns, per fire time, how many runs the claims recorded and how many multi-jobs the schedule got.
func scheduleRuns(t *testing.T, d *pgtest.DB, id string) (claims map[string]string, jobs int) {
	t.Helper()
	var rows []struct {
		Fire   time.Time `db:"fire_time"`
		Holder string    `db:"holder"`
	}
	require.NoError(t, d.Owner.Select(&rows, "SELECT fire_time, holder FROM schedule_runs WHERE schedule_id = $1", id))
	claims = map[string]string{}
	for _, r := range rows {
		claims[r.Fire.UTC().Format(time.RFC3339Nano)] = r.Holder
	}
	require.NoError(t, d.Owner.Get(&jobs, "SELECT count(*) FROM multi_jobs WHERE schedule_id = $1", id))
	return claims, jobs
}

func TestScheduleCreatedOnAPINodeRunsOnceOnListenerNode(t *testing.T) {
	d := pgtest.New(t)
	seedTenants(t, d)
	listener := startNodeOn(t, d, nodeSettings{nodeID: "node-a", role: chconfig.StorageRoleAll})
	api := startNodeOn(t, d, nodeSettings{nodeID: "node-b"})
	alice := api.login(t, "alice")
	code, out := api.do(t, http.MethodPost, "/schedules", alice, map[string]any{
		"name": "every 2s", "schedule": "@every 2s", "type": "command", "command": "uptime",
		"client_ids": []string{"web01"}, "overlaps": true,
	})
	require.Less(t, code, 300, out["_raw"])
	id := out["data"].(map[string]any)["id"].(string)
	_ = listener

	// both nodes run the schedule's cron (the listener node learned it from the feed); only the node that can
	// reach the clients claims the runs, each fire time once
	require.Eventually(t, func() bool {
		claims, _ := scheduleRuns(t, d, id)
		return len(claims) >= 3
	}, 30*time.Second, 200*time.Millisecond)
	time.Sleep(time.Second) // let the last claimed run record its multi-job
	claims, jobs := scheduleRuns(t, d, id)
	for fire, h := range claims {
		assert.True(t, strings.HasPrefix(h, "node-a/"), "run %s claimed by %s", fire, h)
	}
	assert.InDelta(t, len(claims), jobs, 1, "one multi-job per claimed fire time (the newest may still be starting)")
}

func TestScheduleOfOtherTenantRunsAfterRestart(t *testing.T) {
	d := pgtest.New(t)
	seedTenants(t, d)
	first := startNodeOn(t, d, nodeSettings{nodeID: "node-a", role: chconfig.StorageRoleAll})
	bob := first.login(t, "bob") // tenant b, not the default one
	code, out := first.do(t, http.MethodPost, "/schedules", bob, map[string]any{
		"name": "b's", "schedule": "@every 1s", "type": "command", "command": "uptime",
		"client_ids": []string{"web01"}, "overlaps": true,
	})
	require.Less(t, code, 300, out["_raw"])
	id := out["data"].(map[string]any)["id"].(string)
	first.stop()

	startNodeOn(t, d, nodeSettings{nodeID: "node-a", role: chconfig.StorageRoleAll})
	restarted := time.Now()
	require.Eventually(t, func() bool {
		claims, _ := scheduleRuns(t, d, id)
		for fire := range claims {
			if ft, _ := time.Parse(time.RFC3339Nano, fire); ft.After(restarted) {
				return true
			}
		}
		return false
	}, 15*time.Second, 200*time.Millisecond, "the schedules of every tenant are loaded at start")
}

func TestTwoNodesStartingTogetherSyncBuiltinsOnce(t *testing.T) {
	d := pgtest.New(t)
	d.AddTenants(t, "fresh") // created without the built-ins (AddTenant would sync them)
	var wg sync.WaitGroup
	for _, n := range []string{"node-a", "node-b", "node-c"} {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			startNodeOn(t, d, nodeSettings{nodeID: n})
		}(n)
	}
	wg.Wait()
	builtins, err := library.Builtins()
	require.NoError(t, err)
	for _, tid := range []string{"default", "fresh"} {
		var n, distinct int
		require.NoError(t, d.Owner.Get(&n, "SELECT count(*) FROM library_entries WHERE tenant_id = $1 AND builtin_key IS NOT NULL", tid))
		require.NoError(t, d.Owner.Get(&distinct, "SELECT count(DISTINCT builtin_key) FROM library_entries WHERE tenant_id = $1", tid))
		assert.Equal(t, len(builtins), n, "tenant %s: every built-in, synced by one node", tid)
		assert.Equal(t, n, distinct, tid)
	}
	var holders int
	require.NoError(t, d.Owner.Get(&holders, "SELECT count(DISTINCT holder) FROM work_leases WHERE name LIKE 'library-builtins/%' AND tenant_id = 'fresh'"))
	assert.Equal(t, 1, holders)
}

func TestSecondClientListenerNodeRefused(t *testing.T) {
	d := pgtest.New(t)
	startNodeOn(t, d, nodeSettings{nodeID: "node-a", role: chconfig.StorageRoleAll})
	cfg := loadConfig(t, nodeConf(t, d.AppURL, freeAddr(t), nodeSettings{nodeID: "node-b", role: chconfig.StorageRoleAll}))
	_, _, err := NodeOpts(context.Background(), cfg, d.App, &chserver.ServerOpts{FilesAPI: files.NewFileSystem()}, io.Discard)
	assert.ErrorContains(t, err, `node "node-a" accepts the client connections`)
	cfg = loadConfig(t, nodeConf(t, d.AppURL, freeAddr(t), nodeSettings{nodeID: "node-c", role: chconfig.StorageRoleAPI}))
	_, release, err := NodeOpts(context.Background(), cfg, d.App, &chserver.ServerOpts{FilesAPI: files.NewFileSystem()}, io.Discard)
	require.NoError(t, err, "api nodes are not limited")
	release()
}

func TestClientListenerLeaseFailsOverWhenNodeStops(t *testing.T) {
	ttl, renew := clientListenerTTL, clientListenerRenew
	clientListenerTTL, clientListenerRenew = 2*time.Second, 300*time.Millisecond
	t.Cleanup(func() { clientListenerTTL, clientListenerRenew = ttl, renew })

	d := pgtest.New(t)
	a := startNodeOn(t, d, nodeSettings{nodeID: "node-a", role: chconfig.StorageRoleAll})
	cfg := loadConfig(t, nodeConf(t, d.AppURL, freeAddr(t), nodeSettings{nodeID: "node-b", role: chconfig.StorageRoleAll}))
	tryB := func() error {
		_, release, err := NodeOpts(context.Background(), cfg, d.App, &chserver.ServerOpts{FilesAPI: files.NewFileSystem()}, io.Discard)
		if err == nil {
			release()
		}
		return err
	}
	time.Sleep(3 * clientListenerTTL / 2)
	require.Error(t, tryB(), "a running node keeps renewing its lease past the TTL")

	a.stop()
	stopped := time.Now()
	require.Eventually(t, func() bool { return tryB() == nil }, 10*time.Second, 200*time.Millisecond, "B takes over once A's lease expired")
	assert.Less(t, time.Since(stopped), clientListenerTTL+2*time.Second)
}

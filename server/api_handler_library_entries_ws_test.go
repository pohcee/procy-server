package chserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api"
	"github.com/openrport/openrport/share/models"
	"github.com/openrport/openrport/share/ws"
)

func (e *libraryRunEnv) dialLibraryWS(t *testing.T) *websocket.Conn {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.al.handleLibraryEntriesWS(w, r.WithContext(api.WithUser(r.Context(), "admin")))
	})
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	conn, _, err := websocket.DefaultDialer.Dial(httpToWS(t, s.URL), nil)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	return conn
}

func readWSMap(t *testing.T, conn *websocket.Conn) map[string]interface{} {
	t.Helper()
	_, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(msg, &m), string(msg))
	return m
}

func TestWSRunEntryStreamsResults(t *testing.T) {
	e := newLibraryRunEnv(t)
	entry := e.createEntry(t, runEntryBody)
	conn := e.dialLibraryWS(t)

	require.NoError(t, conn.WriteJSON(map[string]interface{}{
		"entry_id":             entry.ID,
		"client_ids":           []string{"c-linux", "c-mac"},
		"execute_concurrently": true,
	}))

	// the device without an implementation is reported like a finished job
	skipped := readWSMap(t, conn)
	assert.Equal(t, "skipped", skipped["status"])
	assert.Equal(t, "c-mac", skipped["client_id"])
	assert.Equal(t, entry.ID, skipped["library_entry_id"])
	assert.EqualValues(t, 1, skipped["library_version"])
	assert.Equal(t, "darwin", skipped["os"])
	assert.Equal(t, "no macOS implementation", skipped["result"].(map[string]interface{})["summary"])
	assert.NotNil(t, skipped["finished_at"])
	mjid, _ := skipped["multi_job_id"].(string)
	require.NotEmpty(t, mjid)

	mj := e.waitJobs(t, mjid, 2)
	var linuxJID string
	for _, j := range mj.Jobs {
		if j.ClientID == "c-linux" {
			linuxJID = j.JID
		}
	}
	require.NotEmpty(t, linuxJID)

	// a result from an old client that does not echo the library fields
	cl := &ClientListener{logger: testLog, server: e.al.Server}
	res := `{"jid":"` + linuxJID + `","multi_job_id":"` + mjid + `","client_id":"c-linux","status":"successful","started_at":"2026-01-01T00:00:00Z","result":{"stdout":"up 1 day"}}`
	_, err := cl.saveCmdResult([]byte(res))
	require.NoError(t, err)

	live := readWSMap(t, conn)
	assert.Equal(t, "successful", live["status"])
	assert.Equal(t, entry.ID, live["library_entry_id"])
	assert.EqualValues(t, 1, live["library_version"])
	assert.Equal(t, "linux", live["os"])
	assert.Equal(t, "up 1 day", live["result"].(map[string]interface{})["stdout"])
}

func TestWSRunEntryUnknown(t *testing.T) {
	e := newLibraryRunEnv(t)
	conn := e.dialLibraryWS(t)

	require.NoError(t, conn.WriteJSON(map[string]interface{}{
		"entry_id":   "nope",
		"client_ids": []string{"c-linux"},
	}))
	m := readWSMap(t, conn)
	errs, ok := m["errors"].([]interface{})
	require.True(t, ok, "%v", m)
	assert.Contains(t, errs[0].(map[string]interface{})["detail"], "not found")
}

func TestWSRunEntryNoTargets(t *testing.T) {
	e := newLibraryRunEnv(t)
	entry := e.createEntry(t, runEntryBody)
	conn := e.dialLibraryWS(t)

	require.NoError(t, conn.WriteJSON(map[string]interface{}{"entry_id": entry.ID}))
	m := readWSMap(t, conn)
	assert.NotNil(t, m["errors"], "%v", m)
}

func TestTypedResultForwardedUntouched(t *testing.T) {
	e := newLibraryRunEnv(t)
	require.NoError(t, e.jp.CreateJob(&models.Job{JID: "t1", ClientID: "c-linux", Status: models.JobStatusRunning, StartedAt: time.Now()}))

	received := make(chan []byte, 1)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := apiUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		cts := newTestConcurrentWS(conn, e.al)
		e.al.Server.uiJobWebSockets.Set("t1", cts)
		<-r.Context().Done()
	}))
	defer s.Close()
	c, _, err := websocket.DefaultDialer.Dial(httpToWS(t, s.URL), nil)
	require.NoError(t, err)
	defer c.Close()
	require.Eventually(t, func() bool { return e.al.Server.uiJobWebSockets.Get("t1") != nil }, 5*time.Second, 10*time.Millisecond)
	go func() {
		_, m, _ := c.ReadMessage()
		received <- m
	}()

	raw := `{"jid":"t1","client_id":"c-linux","status":"successful",  "started_at":"2026-01-01T00:00:00Z"}`
	cl := &ClientListener{logger: testLog, server: e.al.Server}
	_, err = cl.saveCmdResult([]byte(raw))
	require.NoError(t, err)
	select {
	case m := <-received:
		assert.Equal(t, raw, string(m))
	case <-time.After(5 * time.Second):
		t.Fatal("no message")
	}
}

func newTestConcurrentWS(conn *websocket.Conn, al *APIListener) *ws.ConcurrentWebSocket {
	return ws.NewConcurrentWebSocket(conn, al.Logger)
}

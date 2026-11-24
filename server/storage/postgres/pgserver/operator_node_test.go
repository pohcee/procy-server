package pgserver

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func data(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	d, ok := out["data"].(map[string]any)
	require.True(t, ok, out["_raw"])
	return d
}

func capabilities(t *testing.T, h *nodeFixture, tok string) map[string]any {
	t.Helper()
	code, out := h.do(t, http.MethodGet, "/status", tok, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	return data(t, out)["capabilities"].(map[string]any)
}

func TestNodeOperatorAPI(t *testing.T) {
	h := startNode(t)
	op := h.login(t, "op")
	assert.Equal(t, true, capabilities(t, h, op)["operator"])

	code, out := h.do(t, http.MethodPost, "/operator/organizations", op, map[string]any{"id": "acme", "name": "Acme", "device_limit": 3})
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	assert.Equal(t, "acme", data(t, out)["id"])
	assert.EqualValues(t, 3, data(t, out)["device_limit"])

	code, out = h.do(t, http.MethodGet, "/operator/organizations", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	byID := map[string]map[string]any{}
	for _, o := range out["data"].([]any) {
		m := o.(map[string]any)
		byID[m["id"].(string)] = m
	}
	assert.Contains(t, byID, "default")
	assert.Contains(t, byID, "a")
	assert.NotContains(t, byID, "_system")
	assert.EqualValues(t, 1, byID["a"]["device_count"], out["_raw"])
	assert.EqualValues(t, 1, byID["a"]["user_count"], out["_raw"])
	assert.Nil(t, byID["a"]["device_limit"])
	assert.Equal(t, "active", byID["acme"]["status"])

	code, out = h.do(t, http.MethodPut, "/operator/organizations/acme/device-limit", op, map[string]any{"device_limit": 7})
	require.Equal(t, http.StatusOK, code, out["_raw"])
	code, out = h.do(t, http.MethodGet, "/operator/organizations/acme", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.EqualValues(t, 7, data(t, out)["device_limit"], "limit read back")

	code, out = h.do(t, http.MethodPost, "/operator/organizations/acme/admins", op, map[string]any{"username": "carol"})
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	pw := data(t, out)["password"].(string)
	require.NotEmpty(t, pw)

	// the first login sets a new password
	code, out = h.do(t, http.MethodPost, "/login", "", map[string]string{"username": "carol", "password": pw, "new_password": testPassword})
	require.Equal(t, http.StatusOK, code, out["_raw"])
	carol := data(t, out)["token"].(string)
	code, out = h.do(t, http.MethodGet, "/me", carol, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, []any{"Administrators"}, data(t, out)["groups"])
	code, out = h.do(t, http.MethodGet, "/clients", carol, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Empty(t, out["data"])
	_, hasOperator := capabilities(t, h, carol)["operator"]
	assert.False(t, hasOperator, "an organization admin is not an operator")

	// suspension blocks the organization's sessions and logins at once
	code, out = h.do(t, http.MethodPost, "/operator/organizations/acme/suspend", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, "suspended", data(t, out)["status"])
	code, out = h.do(t, http.MethodGet, "/me", carol, nil)
	assert.Equal(t, http.StatusUnauthorized, code, out["_raw"])

	code, out = h.do(t, http.MethodPost, "/operator/organizations/acme/activate", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	code, out = h.do(t, http.MethodGet, "/me", carol, nil)
	assert.Equal(t, http.StatusOK, code, out["_raw"], "reactivated: the session works again")

	var actions []string
	require.NoError(t, h.db.Owner.Select(&actions,
		"SELECT application || ' ' || action FROM auditlog WHERE tenant_id = '_system' AND affected_id = 'acme' AND username = 'op' ORDER BY id"))
	assert.Equal(t, []string{
		"operator.organization create", "operator.organization update", "operator.organization.admin create",
		"operator.organization suspend", "operator.organization activate",
	}, actions)
}

func TestNodeOperatorLoginToSuspendedOrganization(t *testing.T) {
	h := startNode(t)
	op := h.login(t, "op")
	code, out := h.do(t, http.MethodPost, "/operator/organizations/a/suspend", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	code, out = h.do(t, http.MethodPost, "/login", "", map[string]string{"username": "alice", "password": testPassword})
	assert.Equal(t, http.StatusUnauthorized, code, out["_raw"])
}

func TestNodeOperatorAPIRefusesOthers(t *testing.T) {
	h := startNode(t)
	alice, viewer := h.login(t, "alice"), h.login(t, "viewer")

	code, out := h.do(t, http.MethodGet, "/operator/organizations", alice, nil)
	assert.Equal(t, http.StatusNotFound, code, "an organization admin: %s", out["_raw"])
	code, out = h.do(t, http.MethodPost, "/operator/organizations", alice, map[string]any{"id": "evil"})
	assert.Equal(t, http.StatusNotFound, code, out["_raw"])
	code, out = h.do(t, http.MethodGet, "/operator/organizations", viewer, nil)
	assert.Equal(t, http.StatusForbidden, code, "a non-admin of the system tenant: %s", out["_raw"])
	_, hasOperator := capabilities(t, h, alice)["operator"]
	assert.False(t, hasOperator)
	_, hasOperator = capabilities(t, h, viewer)["operator"]
	assert.False(t, hasOperator)

	// operators see no organization's data through the normal API
	op := h.login(t, "op")
	for _, path := range []string{"/clients", "/clients-auth", "/vault", "/library/entries", "/schedules"} {
		code, out = h.do(t, http.MethodGet, path, op, nil)
		assert.Equal(t, http.StatusForbidden, code, "%s: %s", path, out["_raw"])
	}
	// websockets too (they authenticate themselves, outside the guarded routes)
	code, out = h.do(t, http.MethodGet, "/ws/library-entries?access_token="+op, "", nil)
	assert.Equal(t, http.StatusForbidden, code, out["_raw"])
	code, out = h.do(t, http.MethodGet, "/users", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Len(t, out["data"], 2, "only the operators themselves: %s", out["_raw"])
	code, out = h.do(t, http.MethodGet, "/me", op, nil)
	assert.Equal(t, http.StatusOK, code, out["_raw"])

	code, out = h.do(t, http.MethodGet, "/operator/organizations", "", nil)
	assert.Equal(t, http.StatusUnauthorized, code, out["_raw"])
}

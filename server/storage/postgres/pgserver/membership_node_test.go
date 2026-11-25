package pgserver

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loginAs posts a login with body and returns the status and the data of the response.
func (h *nodeFixture) loginAs(t *testing.T, body map[string]string) (int, map[string]any) {
	t.Helper()
	code, out := h.do(t, http.MethodPost, "/login", "", body)
	data, _ := out["data"].(map[string]any)
	if data == nil {
		data = map[string]any{"_raw": out["_raw"]}
	}
	return code, data
}

// basic sends a request with basic auth and returns the status and the body.
func (h *nodeFixture) basic(t *testing.T, method, path, user, pass string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, h.base+path, nil)
	require.NoError(t, err)
	req.SetBasicAuth(user, pass)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	out["_raw"] = string(raw)
	return resp.StatusCode, out
}

func orgIDs(t *testing.T, v any) []string {
	t.Helper()
	list, ok := v.([]any)
	require.True(t, ok, "%v", v)
	var ids []string
	for _, o := range list {
		ids = append(ids, o.(map[string]any)["id"].(string))
	}
	return ids
}

// One account, member of two organizations: the login lists both, picking one scopes everything, switching issues
// a token of the other while the old one keeps its organization, API tokens stay in their organization, and removing
// one membership blocks that organization only.
func TestNodeAccountInTwoOrganizations(t *testing.T) {
	h := startNode(t) // a: alice (admin), b: bob (admin)
	bob := h.login(t, "bob")

	code, out := h.do(t, http.MethodGet, "/status", bob, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, true, out["data"].(map[string]any)["capabilities"].(map[string]any)["organizations"])

	// a single-organization login sees no change
	code, data := h.loginAs(t, map[string]string{"username": "bob", "password": testPassword})
	require.Equal(t, http.StatusOK, code, data)
	assert.NotContains(t, data, "organizations")
	assert.NotContains(t, data, "organization")

	// bob invites alice (an account of a) into b: she joins at once with her own password, as a Viewer
	code, out = h.do(t, http.MethodPost, "/organization/members", bob, map[string]any{"username": "alice", "groups": []string{"Viewers"}})
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	assert.Equal(t, map[string]any{"organization": "b", "username": "alice", "new_account": false, "password_expired": false}, out["data"])
	code, out = h.do(t, http.MethodPost, "/organization/members", bob, map[string]any{"username": "alice"})
	assert.Equal(t, http.StatusConflict, code, out["_raw"])

	// a login naming no organization gets the list and no token, before 2FA
	code, data = h.loginAs(t, map[string]string{"username": "alice", "password": testPassword})
	require.Equal(t, http.StatusOK, code, data)
	assert.Equal(t, []string{"a", "b"}, orgIDs(t, data["organizations"]))
	assert.Nil(t, data["token"])
	assert.Nil(t, data["two_fa"])
	assert.NotContains(t, data, "organization")

	// the client repeats it with the organization query parameter
	code, out = h.do(t, http.MethodPost, "/login?organization=a", "", map[string]string{"username": "alice", "password": testPassword})
	require.Equal(t, http.StatusOK, code, out["_raw"])
	data = out["data"].(map[string]any)
	assert.Equal(t, "a", data["organization"])
	assert.Equal(t, []string{"a", "b"}, orgIDs(t, data["organizations"]))
	tokA := data["token"].(string)
	code, out = h.do(t, http.MethodGet, "/clients", tokA, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, []string{"a-web"}, clientNames(t, out))

	// picking b scopes everything to b, with b's groups
	code, data = h.loginAs(t, map[string]string{"username": "alice", "password": testPassword, "organization": "b"})
	require.Equal(t, http.StatusOK, code, data)
	assert.Equal(t, "b", data["organization"])
	tokB := data["token"].(string)
	code, out = h.do(t, http.MethodGet, "/me", tokB, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, []any{"Viewers"}, out["data"].(map[string]any)["groups"])
	code, out = h.do(t, http.MethodGet, "/users", tokB, nil)
	assert.Equal(t, http.StatusForbidden, code, "not an administrator in b: %s", out["_raw"])
	code, out = h.do(t, http.MethodGet, "/users", tokA, nil)
	assert.Equal(t, http.StatusOK, code, "an administrator in a: %s", out["_raw"])

	// switching from a gives a token of b; the token of a stays in a
	code, out = h.do(t, http.MethodGet, "/me/organizations", tokA, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, []any{
		map[string]any{"id": "a", "name": "a", "current": true},
		map[string]any{"id": "b", "name": "b", "current": false},
	}, out["data"])
	code, out = h.do(t, http.MethodPost, "/me/organization", tokA, map[string]string{"organization": "b"})
	require.Equal(t, http.StatusOK, code, out["_raw"])
	switched := out["data"].(map[string]any)["token"].(string)
	code, out = h.do(t, http.MethodGet, "/me/organizations", switched, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, true, out["data"].([]any)[1].(map[string]any)["current"])
	code, out = h.do(t, http.MethodGet, "/users", switched, nil)
	assert.Equal(t, http.StatusForbidden, code, "the switched token is b's: %s", out["_raw"])
	code, out = h.do(t, http.MethodGet, "/clients", tokA, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, []string{"a-web"}, clientNames(t, out), "the old token keeps its organization")
	for _, org := range []string{"_system", "nowhere", "_unresolved"} {
		code, out = h.do(t, http.MethodPost, "/me/organization", tokA, map[string]string{"organization": org})
		assert.Equal(t, http.StatusForbidden, code, "%s: %s", org, out["_raw"])
	}

	// an API token made in b works in b only, and can't switch
	code, out = h.do(t, http.MethodPost, "/me/tokens", tokB, map[string]string{"scope": "read+write", "name": "ci"})
	require.Equal(t, http.StatusOK, code, out["_raw"])
	apiTokenB := out["data"].(map[string]any)["token"].(string)
	code, data = h.loginAs(t, map[string]string{"username": "alice", "password": testPassword, "organization": "a"})
	require.Equal(t, http.StatusOK, code, data)
	assert.Equal(t, "a", data["organization"], "a is remembered now")
	code, out = h.basic(t, http.MethodGet, "/me", "alice", testPassword)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, []any{"Administrators"}, out["data"].(map[string]any)["groups"], "basic auth with the password: the remembered organization")
	code, out = h.basic(t, http.MethodGet, "/me", "alice", apiTokenB)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, []any{"Viewers"}, out["data"].(map[string]any)["groups"], "the token's organization, not the remembered one")
	code, out = h.basic(t, http.MethodPost, "/me/organization", "alice", apiTokenB)
	assert.Equal(t, http.StatusForbidden, code, out["_raw"])

	// bob can't change the shared account's password, only its membership in b
	code, out = h.do(t, http.MethodPut, "/users/alice", bob, map[string]any{"password": "a whole new password here"})
	assert.Equal(t, http.StatusForbidden, code, out["_raw"])
	code, out = h.do(t, http.MethodPut, "/users/alice", bob, map[string]any{"groups": []string{"Viewers", "Operators"}})
	assert.Equal(t, http.StatusNoContent, code, out["_raw"])

	// the operator sees the membership
	op := h.login(t, "op")
	code, out = h.do(t, http.MethodGet, "/operator/organizations/b/members", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, []any{
		map[string]any{"username": "alice", "groups": []any{"Operators", "Viewers"}, "organizations": float64(2)},
		map[string]any{"username": "bob", "groups": []any{"Administrators"}, "organizations": float64(1)},
	}, out["data"])

	// removing alice from b blocks b only
	code, out = h.do(t, http.MethodDelete, "/users/alice", bob, nil)
	require.Equal(t, http.StatusNoContent, code, out["_raw"])
	// a stays as it was
	code, out = h.do(t, http.MethodGet, "/clients", tokA, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, []string{"a-web"}, clientNames(t, out))
	code, data = h.loginAs(t, map[string]string{"username": "alice", "password": testPassword})
	require.Equal(t, http.StatusOK, code, data)
	assert.NotContains(t, data, "organizations", "one organization again: as before")
	code, out = h.do(t, http.MethodGet, "/clients", data["token"].(string), nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, []string{"a-web"}, clientNames(t, out))

	// b's session tokens, API token and login fail; after the first failure the login is held back for a while
	// (user_login_wait), so later ones may answer 429
	refused := []int{http.StatusUnauthorized, http.StatusTooManyRequests}
	code, out = h.do(t, http.MethodGet, "/me", tokB, nil)
	assert.Equal(t, http.StatusUnauthorized, code, out["_raw"])
	code, out = h.do(t, http.MethodGet, "/me", switched, nil)
	assert.Contains(t, refused, code, out["_raw"])
	code, out = h.basic(t, http.MethodGet, "/me", "alice", apiTokenB)
	assert.Contains(t, refused, code, out["_raw"])
	code, data = h.loginAs(t, map[string]string{"username": "alice", "password": testPassword, "organization": "b"})
	assert.Contains(t, refused, code, data)
}

// Inviting a login that has no account creates one with a one-time password; a suspended organization drops out of
// the login's list.
func TestNodeInviteNewLoginAndSuspendedOrganization(t *testing.T) {
	h := startNode(t)
	bob := h.login(t, "bob")
	code, out := h.do(t, http.MethodPost, "/organization/members", bob, map[string]any{"username": "dave@example.com", "groups": []string{"Administrators"}})
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	invited := out["data"].(map[string]any)
	assert.Equal(t, true, invited["new_account"])
	assert.Equal(t, true, invited["password_expired"])
	password := invited["password"].(string)
	require.NotEmpty(t, password)

	code, data := h.loginAs(t, map[string]string{"username": "dave@example.com", "password": password, "new_password": "dave's own long password"})
	require.Equal(t, http.StatusOK, code, data)
	dave := data["token"].(string)
	code, out = h.do(t, http.MethodGet, "/clients", dave, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, []string{"b-web"}, clientNames(t, out))

	// alice (admin of a) adds dave to a: a's admins invite by login; dave picks between a and b
	alice := h.login(t, "alice")
	code, out = h.do(t, http.MethodPost, "/organization/members", alice, map[string]any{"username": "dave@example.com", "groups": []string{"Administrators"}})
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	code, data = h.loginAs(t, map[string]string{"username": "dave@example.com", "password": "dave's own long password"})
	require.Equal(t, http.StatusOK, code, data)
	assert.Equal(t, []string{"a", "b"}, orgIDs(t, data["organizations"]))

	// the operator suspends a: dave's login goes to b and lists b alone
	op := h.login(t, "op")
	code, out = h.do(t, http.MethodPost, "/operator/organizations/a/suspend", op, nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	code, data = h.loginAs(t, map[string]string{"username": "dave@example.com", "password": "dave's own long password"})
	require.Equal(t, http.StatusOK, code, data)
	assert.NotContains(t, data, "organizations")
	code, out = h.do(t, http.MethodGet, "/clients", data["token"].(string), nil)
	require.Equal(t, http.StatusOK, code, out["_raw"])
	assert.Equal(t, []string{"b-web"}, clientNames(t, out))

	// the operator adds an existing account as an admin: no password, it keeps its own
	code, out = h.do(t, http.MethodPost, "/operator/organizations/b/admins", op, map[string]any{"username": "alice"})
	require.Equal(t, http.StatusCreated, code, out["_raw"])
	assert.Equal(t, map[string]any{"organization": "b", "username": "alice", "new_account": false, "password_expired": false}, out["data"])
	code, out = h.do(t, http.MethodPost, "/operator/organizations/b/admins", op, map[string]any{"username": "alice"})
	assert.Equal(t, http.StatusConflict, code, out["_raw"])
}

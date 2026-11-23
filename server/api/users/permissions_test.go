package users

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScriptsRightGrantsMergedRight(t *testing.T) {
	p := NewPermissions("scripts")
	assert.True(t, p.Has(PermissionCommands))
	assert.True(t, p.Has(PermissionScripts))
}

func TestCommandsRightGrantsScripts(t *testing.T) {
	assert.True(t, NewPermissions("commands").Has(PermissionScripts))
}

func TestUnmarshalAcceptsLegacyScripts(t *testing.T) {
	var p Permissions
	require.NoError(t, json.Unmarshal([]byte(`{"scripts":true}`), &p))
	all := p.All()
	assert.True(t, all["commands"])
	_, hasScripts := all["scripts"]
	assert.False(t, hasScripts)
}

func TestValueStoresScriptsWhenCommandsSet(t *testing.T) {
	v, err := NewPermissions("commands", "tunnels").Value()
	require.NoError(t, err)
	var stored map[string]bool
	require.NoError(t, json.Unmarshal([]byte(v.(string)), &stored))
	assert.Equal(t, map[string]bool{"commands": true, "scripts": true, "tunnels": true}, stored)

	// legacy input is normalised, then stored with both keys
	var p Permissions
	require.NoError(t, json.Unmarshal([]byte(`{"scripts":true}`), &p))
	v, err = p.Value()
	require.NoError(t, err)
	stored = nil
	require.NoError(t, json.Unmarshal([]byte(v.(string)), &stored))
	assert.Equal(t, map[string]bool{"commands": true, "scripts": true}, stored)

	// no commands right: no scripts key; the receiver is not modified
	p = NewPermissions("tunnels")
	v, err = p.Value()
	require.NoError(t, err)
	assert.JSONEq(t, `{"tunnels":true}`, v.(string))
}

func TestAPIOutputHasNoScriptsKey(t *testing.T) {
	b, err := json.Marshal(NewPermissions("commands"))
	require.NoError(t, err)
	assert.NotContains(t, string(b), "scripts")
}

func TestStoredPermissionsRoundTrip(t *testing.T) {
	v, err := NewPermissions("commands", "tunnels").Value()
	require.NoError(t, err)
	var p Permissions
	require.NoError(t, p.Scan(v))
	assert.Equal(t, NewPermissions("commands", "tunnels"), p)
}

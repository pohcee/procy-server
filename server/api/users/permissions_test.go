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

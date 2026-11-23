package library

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/api/command"
	"github.com/openrport/openrport/server/script"
)

func TestScriptViewUpdateKeepsOtherOS(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	e, err := m.Create(ctx, Content{Name: "Restart", Implementations: map[string]Implementation{
		OSLinux:   {Kind: KindScript, Code: "systemctl restart x", Interpreter: "bash"},
		OSWindows: {Kind: KindScript, Code: "Restart-Service x", Interpreter: "powershell"},
	}}, "initial", "alice")
	require.NoError(t, err)

	c := ContentFromScript(script.InputScript{Name: "Restart", Script: "service x restart", Interpreter: "sh", Tags: []string{"ops"}, TimoutSec: 30}, e)
	saved, err := m.Save(ctx, e.ID, SaveInput{Content: c, BaseVersion: e.ActiveVersion, Message: "Saved through /library/scripts"}, "bob")
	require.NoError(t, err)

	assert.Equal(t, 2, saved.ActiveVersion)
	assert.Equal(t, "service x restart", saved.Implementations[OSLinux].Code)
	assert.Equal(t, "sh", saved.Implementations[OSLinux].Interpreter)
	assert.Equal(t, 30, saved.Implementations[OSLinux].TimeoutSec)
	assert.Equal(t, "Restart-Service x", saved.Implementations[OSWindows].Code)
	assert.Equal(t, []string{"ops"}, saved.Tags)

	s := ToScript(*saved)
	assert.Equal(t, "service x restart", s.Script)
	assert.Equal(t, e.ID, s.ID)
}

func TestScriptViewListsOnlyScripts(t *testing.T) {
	script1 := Entry{Implementations: map[string]Implementation{OSLinux: {Kind: KindScript, Code: "a"}}}
	cmd1 := Entry{Implementations: map[string]Implementation{OSLinux: {Kind: KindCommand, Code: "/bin/a"}}}
	auto := Entry{Implementations: map[string]Implementation{OSLinux: {Kind: KindAuto, Code: "echo hi\necho there"}}}
	winFirst := Entry{Implementations: map[string]Implementation{
		OSDarwin:  {Kind: KindCommand, Code: "/bin/a"},
		OSWindows: {Kind: KindScript, Code: "b", Interpreter: "powershell"},
	}}
	empty := Entry{}

	isKind := func(e Entry, k Kind) bool {
		_, impl, ok := Primary(e)
		return ok && impl.EffectiveKind() == k
	}
	assert.True(t, isKind(script1, KindScript))
	assert.False(t, isKind(script1, KindCommand))
	assert.True(t, isKind(cmd1, KindCommand))
	assert.True(t, isKind(auto, KindScript))
	assert.True(t, isKind(winFirst, KindScript), "windows is the primary when there is no linux")
	assert.False(t, isKind(empty, KindScript))

	os, _, _ := Primary(winFirst)
	assert.Equal(t, OSWindows, os)

	m, _ := newTestManager(t)
	ctx := context.Background()
	sc, err := m.Create(ctx, Content{Name: "S", Implementations: script1.Implementations}, "m", "u")
	require.NoError(t, err)
	cm, err := m.Create(ctx, Content{Name: "C", Implementations: cmd1.Implementations}, "m", "u")
	require.NoError(t, err)
	scripts, err := m.ListViews(ctx, KindScript)
	require.NoError(t, err)
	require.Len(t, scripts, 1)
	assert.Equal(t, sc.ID, scripts[0].ID)
	cmds, err := m.ListViews(ctx, KindCommand)
	require.NoError(t, err)
	require.Len(t, cmds, 1)
	assert.Equal(t, cm.ID, cmds[0].ID)
	_, err = m.GetView(ctx, cm.ID, KindScript)
	requireStatus(t, err, http.StatusNotFound)

	require.NoError(t, m.Delete(ctx, cm.ID, "u"))
	cmds, err = m.ListViews(ctx, KindCommand)
	require.NoError(t, err)
	assert.Empty(t, cmds, "deleted entries are not listed")

	_, err = m.GetView(ctx, cm.ID, KindScript)
	requireStatus(t, err, http.StatusNotFound)
	got, err := m.GetView(ctx, sc.ID, KindScript)
	require.NoError(t, err)
	assert.Equal(t, sc.ID, got.ID)
}

func TestCommandViewRoundTrip(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := context.Background()
	in := command.InputCommand{Name: "Disk", Cmd: "/bin/df -h", Tags: []string{"b", "a"}, TimoutSec: 15}
	c := ContentFromCommand(in, nil)
	e, err := m.Create(ctx, c, "Saved through /library/commands", "alice")
	require.NoError(t, err)

	cmd := ToCommand(*e)
	assert.Equal(t, e.ID, cmd.ID)
	assert.Equal(t, "Disk", cmd.Name)
	assert.Equal(t, "/bin/df -h", cmd.Cmd)
	assert.Equal(t, []string{"a", "b"}, []string(*cmd.Tags))
	assert.Equal(t, 15, *cmd.TimoutSec)
	assert.Equal(t, "alice", cmd.CreatedBy)
	assert.Equal(t, OSLinux, e.OS[0])
}

func TestScriptCreateOnWindowsInterpreter(t *testing.T) {
	c := ContentFromScript(script.InputScript{Name: "W", Script: "Get-Date", Interpreter: "powershell"}, nil)
	require.Contains(t, c.Implementations, OSWindows)
	assert.NotContains(t, c.Implementations, OSLinux)
	assert.Equal(t, KindScript, c.Implementations[OSWindows].Kind)

	c = ContentFromScript(script.InputScript{Name: "L", Script: "ls", Interpreter: ""}, nil)
	assert.Contains(t, c.Implementations, OSLinux)
}

package library

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func runTestEntry() Entry {
	return Entry{
		ID: "e1", ActiveVersion: 3,
		Implementations: map[string]Implementation{
			OSLinux:   {Kind: KindScript, Code: "echo hi", Interpreter: "default", IsSudo: true, Cwd: "/tmp", TimeoutSec: 30},
			OSWindows: {Kind: KindAuto, Code: "Get-Service", Interpreter: "powershell", TimeoutSec: 120},
		},
	}
}

func TestSpecForLinuxScript(t *testing.T) {
	spec, ok := SpecFor(runTestEntry(), "linux", nil)
	require.True(t, ok)
	assert.Equal(t, JobSpec{
		Command: "echo hi", Interpreter: "", Cwd: "/tmp", TimeoutSec: 30, IsSudo: true, IsScript: true,
		LibraryEntryID: "e1", LibraryVersion: 3, OS: "linux",
	}, spec)
}

func TestSpecForWindowsAuto(t *testing.T) {
	spec, ok := SpecFor(runTestEntry(), "windows", nil)
	require.True(t, ok)
	assert.True(t, spec.IsScript)
	assert.Equal(t, "powershell", spec.Interpreter)
	assert.Equal(t, 120, spec.TimeoutSec)
}

func TestSpecForMissingOS(t *testing.T) {
	_, ok := SpecFor(runTestEntry(), "darwin", nil)
	assert.False(t, ok)
}

func TestSpecForCaseInsensitiveOS(t *testing.T) {
	spec, ok := SpecFor(runTestEntry(), "Linux", nil)
	require.True(t, ok)
	assert.Equal(t, "linux", spec.OS)
}

func TestOSLabel(t *testing.T) {
	assert.Equal(t, "Linux", OSLabel("linux"))
	assert.Equal(t, "Windows", OSLabel("windows"))
	assert.Equal(t, "macOS", OSLabel("darwin"))
	assert.Equal(t, "plan9", OSLabel("plan9"))
}

func TestMaxTimeout(t *testing.T) {
	assert.Equal(t, 120, MaxTimeout(runTestEntry()))
	assert.Equal(t, 0, MaxTimeout(Entry{}))
}

func TestActiveForRunRejectsDeleted(t *testing.T) {
	m, _ := newTestManager(t)
	ctx := testCtx
	e, err := m.Create(ctx, linuxOnly("Disk", "df"), "", "alice")
	require.NoError(t, err)

	got, err := m.ActiveForRun(ctx, e.ID)
	require.NoError(t, err)
	assert.Contains(t, got.Implementations, OSLinux)

	require.NoError(t, m.Delete(ctx, e.ID, "bob"))
	_, err = m.ActiveForRun(ctx, e.ID)
	requireStatus(t, err, http.StatusNotFound)
	_, err = m.ActiveForRun(ctx, "missing")
	requireStatus(t, err, http.StatusNotFound)
}

func serverURLEntry(builtin bool) Entry {
	e := Entry{
		ID: "b1", ActiveVersion: 1,
		Implementations: map[string]Implementation{
			OSLinux:   {Kind: KindScript, Code: "curl {{server_url}}/pairing/update {{other}}", Interpreter: "default"},
			OSWindows: {Kind: KindAuto, Code: "iwr '{{server_url}}/x'", Interpreter: "powershell"},
		},
	}
	if builtin {
		k := "client-update"
		e.BuiltinKey = &k
	}
	return e
}

func TestSpecForSubstitutesServerURL(t *testing.T) {
	vars := map[string]string{"server_url": "https://rport.example.com"}
	spec, ok := SpecFor(serverURLEntry(true), "linux", vars)
	require.True(t, ok)
	assert.Equal(t, "curl https://rport.example.com/pairing/update {{other}}", spec.Command)
	spec, ok = SpecFor(serverURLEntry(true), "windows", vars)
	require.True(t, ok)
	assert.Equal(t, "iwr 'https://rport.example.com/x'", spec.Command)
}

func TestUserEntryNotSubstituted(t *testing.T) {
	spec, ok := SpecFor(serverURLEntry(false), "linux", map[string]string{"server_url": "https://x"})
	require.True(t, ok)
	assert.Equal(t, "curl {{server_url}}/pairing/update {{other}}", spec.Command)
}

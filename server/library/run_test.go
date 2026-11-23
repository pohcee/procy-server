package library

import (
	"context"
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
	spec, ok := SpecFor(runTestEntry(), "linux")
	require.True(t, ok)
	assert.Equal(t, JobSpec{
		Command: "echo hi", Interpreter: "", Cwd: "/tmp", TimeoutSec: 30, IsSudo: true, IsScript: true,
		LibraryEntryID: "e1", LibraryVersion: 3, OS: "linux",
	}, spec)
}

func TestSpecForWindowsAuto(t *testing.T) {
	spec, ok := SpecFor(runTestEntry(), "windows")
	require.True(t, ok)
	assert.True(t, spec.IsScript)
	assert.Equal(t, "powershell", spec.Interpreter)
	assert.Equal(t, 120, spec.TimeoutSec)
}

func TestSpecForMissingOS(t *testing.T) {
	_, ok := SpecFor(runTestEntry(), "darwin")
	assert.False(t, ok)
}

func TestSpecForCaseInsensitiveOS(t *testing.T) {
	spec, ok := SpecFor(runTestEntry(), "Linux")
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
	ctx := context.Background()
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

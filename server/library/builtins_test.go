package library

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuiltinsLoad(t *testing.T) {
	bs, err := Builtins()
	require.NoError(t, err)
	require.Len(t, bs, 3)
	assert.Equal(t, "client-update", bs[0].Key)
	assert.Equal(t, "os-updates", bs[1].Key)
	assert.Equal(t, "server-switch", bs[2].Key)
	assert.Equal(t, "Client update", bs[0].Content.Name)
	assert.Equal(t, "OS updates", bs[1].Content.Name)
	assert.Equal(t, "Switch server", bs[2].Content.Name)

	want := map[string][2]int{"client-update": {300, 300}, "os-updates": {1800, 3600}, "server-switch": {300, 300}}
	for _, b := range bs {
		assert.Contains(t, b.Content.Tags, "procy")
		assert.Contains(t, b.Content.Tags, b.Key)
		l, w := b.Content.Implementations[OSLinux], b.Content.Implementations[OSWindows]
		assert.Equal(t, want[b.Key][0], l.TimeoutSec)
		assert.Equal(t, want[b.Key][1], w.TimeoutSec)
		assert.True(t, l.IsSudo)
		assert.Equal(t, "powershell", w.Interpreter)
		for _, impl := range []Implementation{l, w} {
			assert.NotContains(t, impl.Code, "rport.safebridge.io")
			assert.Equal(t, b.Key != "os-updates", strings.Contains(impl.Code, "{{server_url}}/pairing/update"))
		}
	}
	sw := bs[2].Content.Implementations
	assert.Contains(t, sw[OSLinux].Code, "rport-update.sh -S\n")
	assert.Contains(t, sw[OSWindows].Code, "& $script -s\n")
}

func TestBuiltinsValid(t *testing.T) {
	bs, err := Builtins()
	require.NoError(t, err)
	for _, b := range bs {
		assert.NoError(t, Validate(b.Content), b.Key)
	}
}

func TestLoadBuiltinsRejectsBadManifests(t *testing.T) {
	good := `{"key":"a","name":"A","implementations":{"linux":{"file":"linux.sh","kind":"script","interpreter":"sh"}}}`
	mk := func(dir, manifest string) fstest.MapFS {
		return fstest.MapFS{
			"builtin/" + dir + "/manifest.json": {Data: []byte(manifest)},
			"builtin/" + dir + "/linux.sh":      {Data: []byte("echo hi")},
		}
	}
	_, err := loadBuiltins(mk("a", good))
	require.NoError(t, err)

	for name, fsys := range map[string]fstest.MapFS{
		"empty key":     mk("a", strings.Replace(good, `"key":"a"`, `"key":""`, 1)),
		"key vs dir":    mk("b", good),
		"unknown field": mk("a", strings.Replace(good, `"name"`, `"nmae":"x","name"`, 1)),
	} {
		_, err := loadBuiltins(fsys)
		assert.Error(t, err, name)
	}
}

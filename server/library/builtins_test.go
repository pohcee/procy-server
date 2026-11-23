package library

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuiltinsLoad(t *testing.T) {
	bs, err := Builtins()
	require.NoError(t, err)
	require.Len(t, bs, 2)
	assert.Equal(t, "client-update", bs[0].Key)
	assert.Equal(t, "os-updates", bs[1].Key)
	assert.Equal(t, "Client update", bs[0].Content.Name)
	assert.Equal(t, "OS updates", bs[1].Content.Name)

	want := map[string][2]int{"client-update": {300, 300}, "os-updates": {1800, 3600}}
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
			assert.Equal(t, b.Key == "client-update", strings.Contains(impl.Code, "{{server_url}}"))
		}
	}
}

func TestBuiltinsValid(t *testing.T) {
	bs, err := Builtins()
	require.NoError(t, err)
	for _, b := range bs {
		assert.NoError(t, Validate(b.Content), b.Key)
	}
}

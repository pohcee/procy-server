package dispatch

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFileGrantsArePerConnectionAndRevocable(t *testing.T) {
	g := NewFileGrants()
	connA, connB := &struct{ n int }{1}, &struct{ n int }{2}

	revoke := g.Grant(connA, "/data/filepush/x_rport_filepush")
	assert.True(t, g.Allowed(connA, "/data/filepush/x_rport_filepush"))
	assert.True(t, g.Allowed(connA, "/data/filepush/../filepush/x_rport_filepush"), "cleaned path")
	assert.False(t, g.Allowed(connB, "/data/filepush/x_rport_filepush"), "another device's connection")
	assert.False(t, g.Allowed(connA, "/data/filepush/y_rport_filepush"))

	revoke2 := g.Grant(connA, "/data/filepush/x_rport_filepush")
	revoke()
	assert.True(t, g.Allowed(connA, "/data/filepush/x_rport_filepush"), "still granted by the second upload")
	revoke2()
	assert.False(t, g.Allowed(connA, "/data/filepush/x_rport_filepush"))
	assert.Empty(t, g.byConn, "no leftovers")
}

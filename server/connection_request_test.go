package chserver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// A connection request that can't be used is answered (the client logs why); a connection closed before it sent
// one has nothing to answer, and the error says so.
func TestReceiveClientConnectionRequest(t *testing.T) {
	cl := deviceTestListener(t, nil, twoTenantAuth())

	for _, tc := range []struct {
		name string
		req  *ssh.Request
		want string
	}{
		{"wrong type", &ssh.Request{Type: "other"}, "expecting connection request"},
		{"invalid payload", &ssh.Request{Type: "new_connection", Payload: []byte("{")}, "invalid connection request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reqs := make(chan *ssh.Request, 1)
			reqs <- tc.req
			_, r, err := cl.receiveClientConnectionRequest(reqs)
			assert.ErrorContains(t, err, tc.want)
			assert.Same(t, tc.req, r, "returned, so the error is sent back to the client")
		})
	}

	reqs := make(chan *ssh.Request)
	close(reqs)
	_, r, err := cl.receiveClientConnectionRequest(reqs)
	require.Error(t, err)
	assert.Nil(t, r)
	assert.ErrorIs(t, err, errNoConnectionRequest)
}

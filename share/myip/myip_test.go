package myip

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const port = 23985

var serverOnce sync.Once

// startServer starts the test API once for the package; both tests use it.
func startServer(t *testing.T) {
	serverOnce.Do(func() { listenAndServe(t) })
}

func listenAndServe(t *testing.T) {
	http.HandleFunc("/good", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, userAgent, r.UserAgent())
		if strings.HasPrefix(r.RemoteAddr, "127.0.0.1") {
			// Handle the ipv4 request
			fmt.Fprintf(w, `{"ip":"127.0.0.1"}`)
			return
		}
		// Handle the ipv6 request
		fmt.Fprintf(w, `{"ip":"::1"}`)
	})
	http.HandleFunc("/bad", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "something went wrong")
	})

	server := &http.Server{
		ReadHeaderTimeout: 3 * time.Second,
	}

	// Listen before returning so the test's first request can't race the server start.
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	require.NoError(t, err)
	go func() { _ = server.Serve(ln) }()
}

func TestGetMyIps(t *testing.T) {
	startServer(t)
	ctx := context.Background()
	ips, err := GetMyIPs(ctx, fmt.Sprintf("http://localhost:%d/good", port))

	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1", ips.IPv4)
	assert.Equal(t, "::1", ips.IPv6)
}

func TestGetMyIpsFailing(t *testing.T) {
	startServer(t)
	ctx := context.Background()
	_, err := GetMyIPs(ctx, fmt.Sprintf("http://localhost:%d/bad", port))

	assert.ErrorContains(t, err, "400: something went wrong")
}

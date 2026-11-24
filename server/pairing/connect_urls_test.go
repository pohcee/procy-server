package pairing

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPublicConnectURLs(t *testing.T) {
	got := PublicConnectURLs([]string{
		"http://connect.procy.sh", "http://0.0.0.0:80", "http://127.0.0.1:80", "http://localhost",
		"http://[::]:80", "http://[::1]:80", "::bad", "", "http://us-central1.connect.procy.sh:80",
	})
	assert.Equal(t, []string{"http://connect.procy.sh", "http://us-central1.connect.procy.sh:80"}, got)
	assert.Empty(t, PublicConnectURLs([]string{"http://0.0.0.0:8080"}))
}

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Windows before Windows 11 / Server 2022 cannot do TLS 1.3. With a TLS 1.3-only API they cannot
// fetch the pairing installer, the update script or the client downloads.
func TestAPIAcceptsTLS12ByDefault(t *testing.T) {
	assert.Equal(t, "1.2", viperCfg.GetString("api.tls_min"))
}

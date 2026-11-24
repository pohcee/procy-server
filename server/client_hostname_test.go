package chserver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClientHostname(t *testing.T) {
	for in, want := range map[string]string{
		"connect.procy.sh":              "connect.procy.sh",
		"Connect.Procy.SH:80":           "connect.procy.sh",
		"us-central1.connect.procy.sh.": "us-central1.connect.procy.sh",
		"[2001:db8::1]:80":              "2001:db8::1",
		"[2001:db8::1]":                 "2001:db8::1",
		"192.0.2.1:8080":                "192.0.2.1",
		"":                              "",
	} {
		assert.Equal(t, want, clientHostname(in), in)
	}
}

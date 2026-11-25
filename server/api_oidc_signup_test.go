package chserver

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSignupOrganizationMarker(t *testing.T) {
	for _, org := range []string{"Acme", "A&B Ünïcode #1 / ?x=y", "100%"} {
		got, ok := signupOrganization(oidcSignupReturn + url.QueryEscape(org))
		assert.True(t, ok, org)
		assert.Equal(t, org, got)
	}
	for _, rt := range []string{"", "/dashboard", "/auth/signup/google", "/auth/signup/googlex?organization=a"} {
		_, ok := signupOrganization(rt)
		assert.False(t, ok, rt)
	}
}

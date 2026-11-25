package signup

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScrubRemovesSecretsAtEveryDepth(t *testing.T) {
	var v any
	require.NoError(t, json.Unmarshal([]byte(`{"id": "t1", "auth_password": "x", "further_options": {"auth_user": "u", "AuthPassword": "y",
		"nested": [{"api_token": "z", "keep": 1}]}, "client_secret": "s", "two_fa_send_to": "a@b.c"}`), &v))
	out, err := json.Marshal(scrub(v))
	require.NoError(t, err)
	assert.JSONEq(t, `{"id": "t1", "further_options": {"auth_user": "u", "nested": [{"keep": 1}]}, "two_fa_send_to": "a@b.c"}`, string(out))
}

package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	errors2 "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/share/logger"
)

// A 500 is always logged (its body alone does not say why); a client error is not.
func TestResponderLogsServerErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	f, err := os.Create(path)
	require.NoError(t, err)
	defer f.Close()
	r := &Responder{Log: logger.NewLogger("api", logger.LogOutput{File: f}, logger.LogLevelInfo)}

	w := httptest.NewRecorder()
	r.Error(w, errors2.APIError{HTTPStatus: http.StatusForbidden, Message: "not allowed"})
	assert.Equal(t, http.StatusForbidden, w.Code)
	w = httptest.NewRecorder()
	r.Error(w, errors.New("the database is gone"))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	w = httptest.NewRecorder()
	r.ErrorWithTitle(w, http.StatusBadGateway, "peer unreachable")

	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(b), "the database is gone")
	assert.Contains(t, string(b), "peer unreachable")
	assert.NotContains(t, string(b), "not allowed")
}

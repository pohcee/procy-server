package chserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/chconfig"
)

// A reload or bookmark of a page of the web UI gets the UI's index.html, the operator console's pages included.
func TestVueHistoryPathsServeTheUI(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.html"), []byte("the ui"), 0o600))
	al := newRouteSnapshotListener(&chconfig.Config{API: chconfig.APIConfig{MaxRequestBytes: 1024 * 1024, DocRoot: dir}})
	al.initRouter()
	for path, want := range map[string]int{
		"/operator":                    http.StatusOK,
		"/operator/organizations/acme": http.StatusOK,
		"/dashboard":                   http.StatusOK,
		"/no-such-page":                http.StatusNotFound,
	} {
		w := httptest.NewRecorder()
		al.router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		assert.Equal(t, want, w.Code, path)
		if want == http.StatusOK {
			assert.Contains(t, w.Body.String(), "the ui", path)
		}
	}
}

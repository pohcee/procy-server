package chserver

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rportplus "github.com/pohcee/procy-server/plus"
	"github.com/pohcee/procy-server/plus/capabilities/oauth"
	"github.com/pohcee/procy-server/server/chconfig"
)

var updateRouteSnapshot = flag.Bool("update-routes", false, "rewrite testdata/api_routes_*.txt")

// routeTable lists every route of r as "METHODS PATH [NAME]", in registration order.
func routeTable(t *testing.T, r *mux.Router) string {
	t.Helper()
	var b strings.Builder
	err := r.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		path, err := route.GetPathTemplate()
		if err != nil {
			path = "-"
		}
		methods, err := route.GetMethods()
		if err != nil || len(methods) == 0 {
			methods = []string{"-"}
		}
		sort.Strings(methods)
		line := strings.Join(methods, ",") + " " + path
		if name := route.GetName(); name != "" {
			line += " [" + name + "]"
		}
		fmt.Fprintln(&b, line)
		return nil
	})
	require.NoError(t, err)
	return b.String()
}

// TestAPIRouteSnapshot pins the API route table (methods, paths and route names) so that moving
// handlers between packages cannot change it. Run with -update-routes to accept a change.
func TestAPIRouteSnapshot(t *testing.T) {
	allOn := &chconfig.Config{
		API: chconfig.APIConfig{
			MaxRequestBytes:       1024 * 1024,
			EnableWsTestEndpoints: true,
			DocRoot:               "/nonexistent-doc-root",
			CORS:                  []string{"https://ui.example.com"},
		},
		Pairing: chconfig.PairingConfig{Enabled: true},
		PlusConfig: rportplus.PlusConfig{
			PluginConfig: &rportplus.PluginConfig{PluginPath: "/nonexistent-plugin"},
			OAuthConfig:  &oauth.Config{},
		},
	}
	allOn.Monitoring.Enabled = true

	cases := map[string]*chconfig.Config{
		"default": {API: chconfig.APIConfig{MaxRequestBytes: 1024 * 1024}},
		"all":     allOn,
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			al := newRouteSnapshotListener(cfg)
			al.initRouter()
			got := routeTable(t, al.router)

			golden := filepath.Join("testdata", "api_routes_"+name+".txt")
			if *updateRouteSnapshot {
				require.NoError(t, os.WriteFile(golden, []byte(got), 0o644)) //nolint:gosec // G306: a committed golden file
			}
			want, err := os.ReadFile(golden)
			require.NoError(t, err)
			assert.Equal(t, string(want), got)
		})
	}
}

// newRouteSnapshotListener is the hand-built API listener the snapshot routes are read from.
func newRouteSnapshotListener(cfg *chconfig.Config) *APIListener {
	return &APIListener{
		insecureForTests: true,
		apiServices:      &apiServices{config: cfg},
		Logger:           testLog,
	}
}

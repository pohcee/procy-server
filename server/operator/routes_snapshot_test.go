package operator

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

	"github.com/pohcee/procy-server/server/billing"
)

var updateRoutes = flag.Bool("update-routes", false, "rewrite testdata/operator_routes.txt")

// TestOperatorRouteSnapshot pins the operator API's routes with every option on (the server's route snapshot runs
// without the operator API). Run with -update-routes to accept a change.
func TestOperatorRouteSnapshot(t *testing.T) {
	h := NewHandler(Options{Service: &fakeService{}, Usage: &fakeUsage{}, Billing: &fakeBilling{}, Plans: &billing.Plans{}, Signup: true, OIDCEnabled: true})
	r := mux.NewRouter()
	h.Mount(r.PathPrefix(RoutePrefix).Subrouter())
	var b strings.Builder
	require.NoError(t, r.Walk(func(route *mux.Route, _ *mux.Router, _ []*mux.Route) error {
		path, err := route.GetPathTemplate()
		if err != nil {
			return nil
		}
		methods, err := route.GetMethods()
		if err != nil {
			return nil
		}
		sort.Strings(methods)
		fmt.Fprintln(&b, strings.Join(methods, ",")+" "+path)
		return nil
	}))
	golden := filepath.Join("testdata", "operator_routes.txt")
	if *updateRoutes {
		require.NoError(t, os.WriteFile(golden, []byte(b.String()), 0o644)) //nolint:gosec // G306: a committed golden file
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err)
	assert.Equal(t, string(want), b.String())
}

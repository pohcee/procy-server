// Package repotest holds contract tests that any implementation of the server's repository interfaces can run
// (#29). Each suite takes a Factory and checks the behavior the server relies on: a context without a tenant is
// refused with tenant.ErrMissing, tenants never see each other's rows, and the round trips the API uses.
//
// The SQLite implementations run every suite in their own tests; another implementation runs the same suites
// against its storage.
package repotest

import (
	"context"
	"testing"

	"github.com/openrport/openrport/share/tenant"
)

// The tenants the suites use. A Factory must return a repository in which both exist.
const (
	TenantA tenant.ID = "a"
	TenantB tenant.ID = "b"
)

// Ctx returns a background context carrying the tenant id.
func Ctx(id tenant.ID) context.Context { return tenant.WithID(context.Background(), id) }

// Factory returns an empty repository in which TenantA and TenantB exist. It is called once per subtest and
// should register any cleanup with t.Cleanup.
type Factory[T any] func(t *testing.T) T

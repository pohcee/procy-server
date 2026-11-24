package pgserver

import (
	"context"
	"net/http"
	"time"

	errors2 "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/server/vault"
	"github.com/openrport/openrport/share/query"
)

// ErrVaultNotAvailable is returned by every vault operation on a Postgres server until the vault is backed by
// managed keys.
var ErrVaultNotAvailable = errors2.APIError{
	Message:    "the vault is not available on this server yet",
	HTTPStatus: http.StatusNotImplemented,
}

// unavailableVault is the vault storage without a managed key provider: it reports the vault as not initialised and refuses to initialise it.
type unavailableVault struct{}

var _ vault.DbProviderFactory = unavailableVault{}

func (unavailableVault) GetDbProvider() vault.DbProvider { return unavailableVaultDB{} }
func (unavailableVault) Init() error                     { return ErrVaultNotAvailable }

type unavailableVaultDB struct{}

var _ vault.DbProvider = unavailableVaultDB{}

func (unavailableVaultDB) GetStatus(context.Context) (vault.DbStatus, error) {
	return vault.DbStatus{}, vault.ErrDatabaseNotInitialised
}
func (unavailableVaultDB) SetStatus(context.Context, vault.DbStatus) error {
	return ErrVaultNotAvailable
}
func (unavailableVaultDB) GetByID(context.Context, int) (vault.StoredValue, bool, error) {
	return vault.StoredValue{}, false, ErrVaultNotAvailable
}
func (unavailableVaultDB) List(context.Context, *query.ListOptions) ([]vault.ValueKey, error) {
	return nil, ErrVaultNotAvailable
}
func (unavailableVaultDB) FindByKeyAndClientID(context.Context, string, string) (vault.StoredValue, bool, error) {
	return vault.StoredValue{}, false, ErrVaultNotAvailable
}
func (unavailableVaultDB) Save(context.Context, string, int64, *vault.InputValue, time.Time) (int64, error) {
	return 0, ErrVaultNotAvailable
}
func (unavailableVaultDB) Delete(context.Context, int) error { return ErrVaultNotAvailable }
func (unavailableVaultDB) Close() error                      { return nil }

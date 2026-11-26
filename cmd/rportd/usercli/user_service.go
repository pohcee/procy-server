package usercli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/pohcee/procy-server/server/api/users"
	"github.com/pohcee/procy-server/server/chconfig"
	"github.com/pohcee/procy-server/server/storage/postgres"
	pgusers "github.com/pohcee/procy-server/server/storage/postgres/users"
	"github.com/pohcee/procy-server/share/enums"
	"github.com/pohcee/procy-server/share/tenant"
)

type UserInput struct {
	Username    string
	Password    string
	Groups      []string
	TwoFASendTo string
}

type UserService interface {
	Create(input UserInput) error
	Change(input UserInput) error
	Delete(username string) error
	ProviderType() enums.ProviderSource
}

// NewUserService returns the service managing the users of the server configured by cfg. tenantID ("" is the default
// tenant) chooses the tenant on a server with [storage] backend = "postgres"; a standalone server has only the
// default tenant.
func NewUserService(cfg *chconfig.Config, tenantID tenant.ID) (UserService, error) {
	if tenantID == "" {
		tenantID = tenant.Default
	}
	if cfg.Storage.IsPostgres() {
		return newPostgresUserService(cfg, tenantID)
	}
	if tenantID != tenant.Default {
		return nil, errors.New(`--tenant is used only with [storage] backend = "postgres"`)
	}

	var authDB *sqlx.DB
	var err error
	if cfg.Database.Driver != "" {
		authDB, err = sqlx.Connect(cfg.Database.Driver, cfg.Database.Dsn)
		if err != nil {
			return nil, fmt.Errorf("Could not connect to user database: %w", err)
		}
	}

	// the CLI manages the users of a standalone server, which belong to the default tenant
	ctx := tenant.WithID(context.Background(), tenant.Default)
	usersProvider, err := users.NewProviderFromConfig(ctx, authDB, cfg)
	if err != nil {
		return nil, fmt.Errorf("Could not initialize user service: %w", err)
	}
	apiUserService := users.NewAPIServiceFromConfig(ctx, usersProvider, cfg)

	return &userServiceImpl{
		ctx:            ctx,
		apiUserService: apiUserService,
	}, nil
}

// newPostgresUserService manages the users of tenant tid in the Postgres database of [storage] dsn (the application
// role). The tenant must exist and be active.
func newPostgresUserService(cfg *chconfig.Config, tid tenant.ID) (UserService, error) {
	ctx := context.Background()
	db, err := postgres.Open(ctx, cfg.Storage.DSN)
	if err != nil {
		return nil, fmt.Errorf("Could not connect to Postgres: %w", err)
	}
	var status string
	if err := db.GetContext(ctx, &status, "SELECT status FROM tenants WHERE id = $1", string(tid)); err != nil {
		db.Close()
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("tenant %q does not exist (add it with rportd tenant add)", tid)
		}
		return nil, fmt.Errorf("tenant %q: %w", tid, err)
	}
	if status != "active" {
		db.Close()
		return nil, fmt.Errorf("tenant %q is %s; users can be managed only in active tenants", tid, status)
	}
	ctx = tenant.WithID(ctx, tid)
	return &userServiceImpl{
		ctx:            ctx,
		apiUserService: users.NewAPIServiceFromConfig(ctx, pgusers.New(db), cfg),
	}, nil
}

type userServiceImpl struct {
	ctx            context.Context
	apiUserService *users.APIService
}

func (u *userServiceImpl) Create(input UserInput) error {
	return u.apiUserService.Change(u.ctx, &users.User{
		Username:    input.Username,
		Password:    input.Password,
		Groups:      input.Groups,
		TwoFASendTo: input.TwoFASendTo,
	}, "")
}

func (u *userServiceImpl) Change(input UserInput) error {
	return u.apiUserService.Change(u.ctx, &users.User{
		Password:    input.Password,
		Groups:      input.Groups,
		TwoFASendTo: input.TwoFASendTo,
	}, input.Username)
}

func (u *userServiceImpl) Delete(username string) error {
	return u.apiUserService.Delete(u.ctx, username)
}

func (u *userServiceImpl) ProviderType() enums.ProviderSource {
	return u.apiUserService.GetProviderType()
}

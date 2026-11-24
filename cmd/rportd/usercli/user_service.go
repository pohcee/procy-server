package usercli

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/server/api/users"
	"github.com/openrport/openrport/server/chconfig"
	"github.com/openrport/openrport/share/enums"
	"github.com/openrport/openrport/share/tenant"
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

func NewUserService(cfg *chconfig.Config) (UserService, error) {
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

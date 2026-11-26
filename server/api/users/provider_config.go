package users

import (
	"context"

	"github.com/jmoiron/sqlx"

	rportplus "github.com/pohcee/procy-server/plus"
	"github.com/pohcee/procy-server/server/chconfig"
	"github.com/pohcee/procy-server/share/logger"
)

// NewProviderFromConfig selects the user provider the config asks for: an auth file, a static user, or the
// [database] user tables on authDB. ctx is the server context: it carries the tenant users from a file are loaded
// into (users from a file reload with it on SIGUSR1). It returns a nil provider when no user source is configured.
func NewProviderFromConfig(ctx context.Context, authDB *sqlx.DB, config *chconfig.Config) (Provider, error) {
	var usersProvider Provider
	var err error
	if rportplus.IsOAuthPermittedUserList(config.PlusConfig) {
		if config.API.AuthFile != "" {
			logger := logger.NewLogger("auth-file", config.Logging.LogOutput, config.Logging.LogLevel)
			usersProvider, err = NewFileAdapter(ctx, logger, NewFileManager(logger, config.API.AuthFile))
			if err != nil {
				return nil, err
			}
		} else if config.API.AuthUserTable != "" {
			logger := logger.NewLogger("database", config.Logging.LogOutput, config.Logging.LogLevel)
			usersProvider, err = newAPIAuthDatabase(authDB, config, logger)
			if err != nil {
				return nil, err
			}
		}
	} else if config.API.AuthFile != "" {
		logger := logger.NewLogger("auth-file", config.Logging.LogOutput, config.Logging.LogLevel)
		usersProvider, err = NewFileAdapter(ctx, logger, NewFileManager(logger, config.API.AuthFile))
		if err != nil {
			return nil, err
		}
	} else if config.API.Auth != "" {
		authUser, e := parseHTTPAuthStr(config.API.Auth)
		if e != nil {
			return nil, e
		}
		// for static user set the admin group
		authUser.Groups = []string{Administrators}
		usersProvider = NewStaticProvider([]*User{authUser})
	} else if config.API.AuthUserTable != "" {
		logger := logger.NewLogger("database", config.Logging.LogOutput, config.Logging.LogLevel)
		usersProvider, err = newAPIAuthDatabase(authDB, config, logger)
		if err != nil {
			return nil, err
		}
	}
	return usersProvider, nil
}

func newAPIAuthDatabase(authDB *sqlx.DB, config *chconfig.Config, logger *logger.Logger) (usersProvider *UserDatabase, err error) {
	usersProvider, err = NewUserDatabase(
		authDB,
		config.API.AuthUserTable,
		config.API.AuthGroupTable,
		config.API.AuthGroupDetailsTable,
		config.API.IsTwoFAOn(),
		config.API.TotPEnabled,
		rportplus.IsPlusEnabled(config.PlusConfig),
		logger,
	)
	return usersProvider, err
}

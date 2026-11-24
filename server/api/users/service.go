package users

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/jmoiron/sqlx"
	zxcvbn "github.com/trustelem/zxcvbn"
	"golang.org/x/crypto/bcrypt"

	rportplus "github.com/openrport/openrport/plus"
	extperm "github.com/openrport/openrport/plus/capabilities/extendedpermission"
	errors2 "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/server/api/message"
	"github.com/openrport/openrport/server/chconfig"
	chshare "github.com/openrport/openrport/share"
	"github.com/openrport/openrport/share/enums"
	"github.com/openrport/openrport/share/logger"
)

type Provider interface {
	Type() enums.ProviderSource
	SupportsGroupPermissions() bool
	GetAll(ctx context.Context) ([]*User, error)
	ListGroups(ctx context.Context) ([]Group, error)
	GetGroup(ctx context.Context, name string) (Group, error)
	UpdateGroup(ctx context.Context, name string, g Group) error
	DeleteGroup(ctx context.Context, name string) error
	GetByUsername(ctx context.Context, username string) (*User, error)
	Add(ctx context.Context, usr *User) error
	Update(ctx context.Context, usr *User, usernameToUpdate string) error
	Delete(ctx context.Context, usernameToDelete string) error
}

type APIService struct {
	DeliverySrv            message.Service
	Provider               Provider
	TwoFAOn                bool
	TotPOn                 bool
	PasswordMinLength      int
	PasswordZxcvbnMinscore int
}

func NewAPIService(provider Provider, twoFAOn bool, passwordMinLength int, PasswordZxcvbnMinscore int) *APIService {
	return &APIService{
		Provider:               provider,
		TwoFAOn:                twoFAOn,
		PasswordMinLength:      passwordMinLength,
		PasswordZxcvbnMinscore: PasswordZxcvbnMinscore,
	}
}

// NewAPIServiceFromConfig builds the user service. ctx is the server context: it carries the tenant users from a
// file are loaded into (users from a file reload with it on SIGUSR1).
func NewAPIServiceFromConfig(ctx context.Context, authDB *sqlx.DB, config *chconfig.Config) (*APIService, error) {
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
	return NewAPIService(
		usersProvider, config.API.IsTwoFAOn(), config.API.PasswordMinLength, config.API.PasswordZxcvbnMinscore,
	), nil
}

func (as APIService) SupportsGroupPermissions() bool {
	return as.Provider.SupportsGroupPermissions()
}

func (as APIService) GetProviderType() enums.ProviderSource {
	return as.Provider.Type()
}

func (as *APIService) GetAll(ctx context.Context) ([]*User, error) {
	return as.Provider.GetAll(ctx)
}

func (as *APIService) GetByUsername(ctx context.Context, username string) (*User, error) {
	return as.Provider.GetByUsername(ctx, username)
}

func (as *APIService) ListGroups(ctx context.Context) ([]Group, error) {
	return as.Provider.ListGroups(ctx)
}

func (as *APIService) GetGroup(ctx context.Context, name string) (Group, error) {
	return as.Provider.GetGroup(ctx, name)
}

func (as *APIService) UpdateGroup(ctx context.Context, name string, g Group) (Group, error) {
	err := as.Provider.UpdateGroup(ctx, name, g)
	if err != nil {
		return Group{}, err
	}
	return as.Provider.GetGroup(ctx, name)
}

func (as *APIService) DeleteGroup(ctx context.Context, name string) error {
	return as.Provider.DeleteGroup(ctx, name)
}

func (as *APIService) ExtendedPermissionsParsingValidation(g Group) error {
	return errors2.APIError{
		Message:    fmt.Sprintf("BAD %v", g.Name),
		HTTPStatus: http.StatusBadRequest,
	}
}
func (as *APIService) CheckPermission(ctx context.Context, user *User, permission string) error {
	for _, groupName := range user.Groups {
		group, err := as.Provider.GetGroup(ctx, groupName)
		if err != nil {
			return err
		}
		if group.Permissions.Has(permission) {
			return nil
		}
	}
	return errors2.APIError{
		Message:    fmt.Sprintf("user does not have %q permission", permission),
		HTTPStatus: http.StatusForbidden,
	}
}

func (as *APIService) GetEffectiveUserExtendedPermissions(ctx context.Context, user *User) ([]extperm.PermissionParams, []extperm.PermissionParams) {
	var tunnelsRestricted []extperm.PermissionParams
	var commandsRestricted []extperm.PermissionParams

	for _, groupName := range user.Groups {
		group, err := as.Provider.GetGroup(ctx, groupName)
		if err == nil {
			if group.TunnelsRestricted != nil {
				tunnelsRestricted = append(tunnelsRestricted, *group.TunnelsRestricted)
			}
			if group.CommandsRestricted != nil {
				commandsRestricted = append(commandsRestricted, *group.CommandsRestricted)
			}
		}
	}
	return tunnelsRestricted, commandsRestricted
}

func (as *APIService) GetEffectiveUserPermissions(ctx context.Context, user *User) (map[string]bool, error) {
	if !as.SupportsGroupPermissions() {
		return NewPermissions(AllPermissions...).All(), nil
	}
	permissions := NewPermissions().All()

	for _, groupName := range user.Groups {
		group, err := as.Provider.GetGroup(ctx, groupName)
		if err != nil {
			return permissions, err
		}
		for _, permission := range AllPermissions {
			if group.Permissions.Has(permission) && !permissions[permission] {
				permissions[permission] = true
			}
		}
	}
	return permissions, nil
}

func (as *APIService) ExistGroups(ctx context.Context, groups []string) error {
	existingGroups, err := as.ListGroups(ctx)
	if err != nil {
		return err
	}

	var groupsNotFound []string
	for _, group := range groups {
		found := false
		for _, existing := range existingGroups {
			if existing.Name == group {
				found = true
				break
			}
		}
		if !found {
			groupsNotFound = append(groupsNotFound, group)
		}
	}

	if len(groupsNotFound) > 0 {
		return errors2.APIError{
			Message:    fmt.Sprintf("user groups not found: %v", strings.Join(groupsNotFound, ", ")),
			HTTPStatus: http.StatusNotFound,
		}
	}

	return nil
}

func (as *APIService) Change(ctx context.Context, usr *User, username string) error {
	err := as.validate(ctx, usr, username)
	if err != nil {
		return err
	}
	if usr.Password != "" {
		passHash, err := GenerateTokenHash(usr.Password)
		if err != nil {
			return err
		}
		usr.Password = passHash
	}

	if username != "" {
		return as.updateUser(ctx, usr, username)
	}
	return as.addUser(ctx, usr)
}

func (as *APIService) validate(ctx context.Context, dataToChange *User, usernameToFind string) error {
	errs := errors2.APIErrors{}
	var zxcvbnUserInputs []string

	if usernameToFind == "" {
		if dataToChange.Username == "" {
			errs = append(errs, errors2.APIError{
				Message:    "username is required",
				HTTPStatus: http.StatusBadRequest,
			})
		}
		if dataToChange.Password == "" {
			errs = append(errs, errors2.APIError{
				Message:    "password is required",
				HTTPStatus: http.StatusBadRequest,
			})
		}
		if as.TwoFAOn && dataToChange.TwoFASendTo == "" {
			errs = append(errs, errors2.APIError{
				Message:    "two_fa_send_to is required",
				HTTPStatus: http.StatusBadRequest,
			})
		}
	} else {
		zxcvbnUserInputs = append(zxcvbnUserInputs, usernameToFind)

		if (dataToChange.Username == "" || dataToChange.Username == usernameToFind) &&
			dataToChange.Password == "" &&
			dataToChange.PasswordExpired == nil &&
			dataToChange.Groups == nil &&
			(!as.TwoFAOn || dataToChange.TwoFASendTo == "") &&
			dataToChange.TotP == "" {
			errs = append(errs, errors2.APIError{
				Message:    "nothing to change",
				HTTPStatus: http.StatusBadRequest,
			})
		}
	}

	if dataToChange.Username != "" {
		zxcvbnUserInputs = append(zxcvbnUserInputs, dataToChange.Username)
		if strings.TrimSpace(dataToChange.Username) != dataToChange.Username {
			errs = append(errs, errors2.APIError{
				Message:    "username must not start or end with whitespace",
				HTTPStatus: http.StatusBadRequest,
			})
		}
	}

	if dataToChange.Password != "" {
		if len(dataToChange.Password) < as.PasswordMinLength {
			errs = append(errs, errors2.APIError{
				Message:    "Your password is too short",                                                // title
				Err:        fmt.Errorf("password must be at least %v characters", as.PasswordMinLength), // detail
				HTTPStatus: http.StatusBadRequest,
			})
		}
		if as.PasswordZxcvbnMinscore >= 0 { // -1 means no zxcvbn
			score := zxcvbn.PasswordStrength(dataToChange.Password, zxcvbnUserInputs)
			if score.Score < as.PasswordZxcvbnMinscore {
				errs = append(errs, errors2.APIError{
					Message:    "Your password is too guessable",                                                              // title
					Err:        fmt.Errorf("zxcvbn score is %v, must be at least %v", score.Score, as.PasswordZxcvbnMinscore), // detail
					HTTPStatus: http.StatusBadRequest,
				})
			}
		}
	}

	if dataToChange.TwoFASendTo != "" && as.DeliverySrv != nil {
		err := as.DeliverySrv.ValidateReceiver(ctx, dataToChange.TwoFASendTo)
		if err != nil {
			errs = append(errs, errors2.APIError{
				Err:        fmt.Errorf("invalid two_fa_send_to: %v", err),
				HTTPStatus: http.StatusBadRequest,
			})
		}
	}

	if len(errs) == 0 {
		return nil
	}

	return errs
}

func (as *APIService) addUser(ctx context.Context, dataToChange *User) error {
	existingUser, err := as.Provider.GetByUsername(ctx, dataToChange.Username)
	if err != nil {
		return err
	}
	if existingUser != nil {
		return errors2.APIError{
			Message:    "Another user with this username already exists",
			HTTPStatus: http.StatusBadRequest,
		}
	}

	err = as.Provider.Add(ctx, dataToChange)
	if err != nil {
		return err
	}

	return nil
}

// todo make concurrent save
func (as *APIService) updateUser(ctx context.Context, dataToChange *User, usernameToFind string) error {
	existingUser, err := as.Provider.GetByUsername(ctx, usernameToFind)
	if err != nil {
		return err
	}

	if existingUser == nil {
		return errors2.APIError{
			Message:    fmt.Sprintf("cannot find user by username '%s'", usernameToFind),
			HTTPStatus: http.StatusNotFound,
		}
	}

	if dataToChange.Username != "" && dataToChange.Username != usernameToFind {
		existingUser, err := as.Provider.GetByUsername(ctx, dataToChange.Username)
		if err != nil {
			return err
		}
		if existingUser != nil {
			return errors2.APIError{
				Message:    "Another user with this username already exists",
				HTTPStatus: http.StatusBadRequest,
			}
		}
	}

	err = as.Provider.Update(ctx, dataToChange, usernameToFind)
	if err != nil {
		return err
	}
	// TODO: if a username changes needs to update username in api tokens table also

	return nil
}

func (as *APIService) Delete(ctx context.Context, usernameToDelete string) error {
	user, err := as.Provider.GetByUsername(ctx, usernameToDelete)
	if err != nil {
		return err
	}
	// TODO: deleting a user needs to delete all his api tokens

	if user == nil {
		return errors2.APIError{
			Message:    fmt.Sprintf("cannot find user by username '%s'", usernameToDelete),
			HTTPStatus: http.StatusNotFound,
		}
	}

	return as.Provider.Delete(ctx, usernameToDelete)
}

func GenerateTokenHash(newTokenClear string) (string, error) {
	tokenHash, err := bcrypt.GenerateFromPassword([]byte(newTokenClear), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	tokenHashStr := strings.Replace(string(tokenHash), HtpasswdBcryptAltPrefix, HtpasswdBcryptPrefix, 1)
	return tokenHashStr, nil
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

// parseHTTPAuthStr parses <user>:<password> string, returns (user, nil) or (nil, error)
func parseHTTPAuthStr(basicAuth string) (*User, error) {
	if basicAuth == "" {
		return nil, nil
	}

	user, pass := chshare.ParseAuth(basicAuth)
	if user == "" || pass == "" {
		return nil, fmt.Errorf("invalid auth format: expected <user>:<password>, actual %s", basicAuth)
	}

	return &User{Username: user, Password: pass}, nil
}

package users

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"

	errors2 "github.com/pohcee/procy-server/server/api/errors"
	"github.com/pohcee/procy-server/share/enums"
	"github.com/pohcee/procy-server/share/logger"
	"github.com/pohcee/procy-server/share/tenant"
)

type FileProvider interface {
	ReadUsersFromFile() ([]*User, error)
	SaveUsersToFile(users []*User) error
}

// FileAdapter serves users from a json file. It is single-tenant: every method accepts only the default tenant
// (tenant.RequireDefault).
type FileAdapter struct {
	*UserCache
	*logger.Logger

	mtx          sync.Mutex
	FileProvider FileProvider
}

// NewFileAdapter loads the users from the file into the tenant of ctx, which must be the default tenant. ctx is the
// server context: the reload on SIGUSR1 runs with it, and stops when it is done.
func NewFileAdapter(ctx context.Context, logger *logger.Logger, fileProvider FileProvider) (*FileAdapter, error) {
	if err := tenant.RequireDefault(ctx); err != nil {
		return nil, fmt.Errorf("users file: %w", err)
	}
	fa := &FileAdapter{
		UserCache:    NewUserCache(nil),
		Logger:       logger,
		FileProvider: fileProvider,
	}
	if err := fa.load(ctx); err != nil {
		return nil, err
	}
	go fa.reload(ctx)
	return fa, nil
}

// load reads users from FileProvider and saves them in cache. It's called from New, on every write and when reload signal is received.
func (fa *FileAdapter) load(ctx context.Context) error {
	users, err := fa.FileProvider.ReadUsersFromFile()
	if err != nil {
		return err
	}
	if err := fa.UserCache.Load(ctx, users); err != nil {
		return err
	}
	fa.Infof("Loaded %v users from file.", len(users))
	return nil
}

// reloadOn reloads the users from file whenever a signal arrives, until ctx is done.
func (fa *FileAdapter) reloadOn(ctx context.Context, signals <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			fa.Infof("Signal SIGUSR1 received. Start to reload API users from file.")

			if err := fa.load(ctx); err != nil {
				fa.Errorf("Failed to reload API users: %v", err)
				continue
			}

			fa.Infof("Finished to reload API users from file.")
		}
	}
}

func (fa *FileAdapter) GetByUsername(ctx context.Context, username string) (*User, error) {
	if err := tenant.RequireDefault(ctx); err != nil {
		return nil, err
	}
	return fa.UserCache.GetByUsername(ctx, username)
}

func (fa *FileAdapter) GetAll(ctx context.Context) ([]*User, error) {
	if err := tenant.RequireDefault(ctx); err != nil {
		return nil, err
	}
	return fa.UserCache.GetAll(ctx)
}

func (fa *FileAdapter) ListGroups(ctx context.Context) ([]Group, error) {
	if err := tenant.RequireDefault(ctx); err != nil {
		return nil, err
	}
	return nil, errors2.APIError{
		Message:    "The json file authentication backend doesn't support this feature.",
		HTTPStatus: http.StatusBadRequest,
	}
}

func (fa *FileAdapter) GetGroup(ctx context.Context, _ string) (Group, error) {
	if err := tenant.RequireDefault(ctx); err != nil {
		return Group{}, err
	}
	return Group{}, errors2.APIError{
		Message:    "The json file authentication backend doesn't support this feature.",
		HTTPStatus: http.StatusBadRequest,
	}
}

func (fa *FileAdapter) UpdateGroup(ctx context.Context, _ string, _ Group) error {
	if err := tenant.RequireDefault(ctx); err != nil {
		return err
	}
	return errors2.APIError{
		Message:    "The json file authentication backend doesn't support this feature.",
		HTTPStatus: http.StatusBadRequest,
	}
}

func (fa *FileAdapter) DeleteGroup(ctx context.Context, _ string) error {
	if err := tenant.RequireDefault(ctx); err != nil {
		return err
	}
	return errors2.APIError{
		Message:    "The json file authentication backend doesn't support this feature.",
		HTTPStatus: http.StatusBadRequest,
	}
}

func (fa *FileAdapter) Delete(ctx context.Context, usernameToDelete string) error {
	if err := tenant.RequireDefault(ctx); err != nil {
		return err
	}
	fa.mtx.Lock()
	defer fa.mtx.Unlock()

	usersFromFile, err := fa.FileProvider.ReadUsersFromFile()
	if err != nil {
		return err
	}
	foundIndex := -1
	for i := range usersFromFile {
		if usersFromFile[i].Username == usernameToDelete {
			foundIndex = i
			break
		}
	}

	if foundIndex < 0 {
		return errors2.APIError{
			Message:    fmt.Sprintf("cannot find user by username '%s'", usernameToDelete),
			HTTPStatus: http.StatusNotFound,
		}
	}

	usersToWriteToFile := append(usersFromFile[:foundIndex], usersFromFile[foundIndex+1:]...)
	err = fa.FileProvider.SaveUsersToFile(usersToWriteToFile)
	if err != nil {
		return err
	}

	return fa.load(ctx)
}

func (fa *FileAdapter) Update(ctx context.Context, dataToChange *User, usernameToFind string) error {
	if err := tenant.RequireDefault(ctx); err != nil {
		return err
	}
	fa.mtx.Lock()
	defer fa.mtx.Unlock()

	users, err := fa.FileProvider.ReadUsersFromFile()
	if err != nil {
		return err
	}

	userFound := -1
	for i := range users {
		if users[i].Username == usernameToFind {
			userFound = i
		}
		if dataToChange.Username != "" && users[i].Username == dataToChange.Username && dataToChange.Username != usernameToFind {
			return errors2.APIError{
				Message:    "Another user with this username already exists",
				HTTPStatus: http.StatusBadRequest,
			}
		}
	}

	if userFound < 0 {
		return errors2.APIError{
			Message:    fmt.Sprintf("cannot find user by username '%s'", usernameToFind),
			HTTPStatus: http.StatusNotFound,
		}
	}

	if dataToChange.Password != "" {
		users[userFound].Password = dataToChange.Password
		users[userFound].PasswordExpired = PasswordExpired(false)
	}
	if dataToChange.PasswordExpired != nil {
		users[userFound].PasswordExpired = dataToChange.PasswordExpired
	}
	if dataToChange.Groups != nil {
		users[userFound].Groups = dataToChange.Groups
	}
	if dataToChange.Username != "" {
		users[userFound].Username = dataToChange.Username
	}
	if dataToChange.TotP != "" {
		users[userFound].TotP = dataToChange.TotP
	}

	err = fa.FileProvider.SaveUsersToFile(users)
	if err != nil {
		return err
	}

	return fa.load(ctx)
}

func (fa *FileAdapter) Add(ctx context.Context, dataToChange *User) error {
	if err := tenant.RequireDefault(ctx); err != nil {
		return err
	}
	fa.mtx.Lock()
	defer fa.mtx.Unlock()

	users, err := fa.FileProvider.ReadUsersFromFile()
	if err != nil {
		return err
	}

	for i := range users {
		if users[i].Username == dataToChange.Username {
			return errors2.APIError{
				Message:    "Another user with this username already exists",
				HTTPStatus: http.StatusBadRequest,
			}
		}
	}

	users = append(users, dataToChange)
	err = fa.FileProvider.SaveUsersToFile(users)
	if err != nil {
		return err
	}

	return fa.load(ctx)
}

func (fa *FileAdapter) Type() enums.ProviderSource {
	return enums.ProviderSourceFile
}

func (fa *FileAdapter) SupportsGroupPermissions() bool {
	return false
}

package vault

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/query"

	errors2 "github.com/openrport/openrport/server/api/errors"
	"github.com/openrport/openrport/share/tenant"
)

var supportedFields = map[string]bool{
	"id":         true,
	"client_id":  true,
	"created_by": true,
	"created_at": true,
	"key":        true,
}

var WrongPasswordError = errors2.APIError{
	Message:    "wrong password provided",
	HTTPStatus: http.StatusUnauthorized,
}

type Config interface {
	GetVaultDBPath() string
}

type UserDataProvider interface {
	GetGroups() []string
	GetUsername() string
}

type DbProvider interface {
	GetStatus(ctx context.Context) (DbStatus, error)
	SetStatus(ctx context.Context, newStatus DbStatus) error
	GetByID(ctx context.Context, id int) (val StoredValue, found bool, err error)
	List(ctx context.Context, lo *query.ListOptions) ([]ValueKey, error)
	FindByKeyAndClientID(ctx context.Context, key, clientID string) (val StoredValue, found bool, err error)
	Save(ctx context.Context, user string, idToUpdate int64, val *InputValue, nowDate time.Time) (int64, error)
	Delete(ctx context.Context, id int) error
	io.Closer
}

type PassManager interface {
	ValidatePass(passToCheck string) error
	PassMatch(dbStatus DbStatus, passToCheck string) (bool, error)
	GetEncRandValue(pass string) (encValue, decValue string, err error)
}

type DbProviderFactory interface {
	GetDbProvider() DbProvider
	Init() error
}

// Manager runs the vault of each tenant. Each tenant has its own status and key, so locking or unlocking one
// tenant's vault doesn't affect another's. Every method needs a tenant in its context.
//
// The key comes from a KeyProvider. With the default PassphraseKeyProvider the vault is initialised, unlocked and
// locked with a passphrase through the API. With any other (managed) provider it needs none of that: Init, UnLock
// and Lock fail with ErrManagedKey and Status reports the vault as set up and managed.
type Manager struct {
	keys KeyProvider
	// passphrase is keys when it is the passphrase provider, nil for a managed provider.
	passphrase *PassphraseKeyProvider
	// dbReady is set once a managed vault has opened its database.
	dbReady atomic.Bool
	// managedTenants holds the tenants whose vault was checked to have no passphrase setup (tenant.ID → struct{}).
	managedTenants sync.Map
	dbFactory      DbProviderFactory
	pm             PassManager
	logger         *logger.Logger
}

// NewManager returns a vault manager whose key comes from a passphrase given through the API.
func NewManager(dbFactory DbProviderFactory, pm PassManager, logger *logger.Logger) *Manager {
	return NewManagerWithKeyProvider(dbFactory, pm, nil, logger)
}

// NewManagerWithKeyProvider returns a vault manager whose key comes from keys. A nil keys, or a
// *PassphraseKeyProvider, gives the passphrase vault of NewManager.
func NewManagerWithKeyProvider(dbFactory DbProviderFactory, pm PassManager, keys KeyProvider, logger *logger.Logger) *Manager {
	if pk, ok := keys.(*PassphraseKeyProvider); keys == nil || (ok && pk == nil) {
		keys = NewPassphraseKeyProvider()
	}
	passphrase, _ := keys.(*PassphraseKeyProvider)
	return &Manager{
		keys:       keys,
		passphrase: passphrase,
		dbFactory:  dbFactory,
		pm:         pm,
		logger:     logger,
	}
}

// UsesPassphrase reports whether the vault is initialised and unlocked with a passphrase through the API.
func (m *Manager) UsesPassphrase() bool {
	return m.passphrase != nil
}

// ErrManagedKey is returned by Init, UnLock and Lock when the vault key comes from a managed KeyProvider.
var ErrManagedKey = errors2.APIError{
	Message:    "the vault key is managed by the server: the vault needs no passphrase and cannot be initialized, unlocked or locked",
	HTTPStatus: http.StatusConflict,
}

func vaultTenant(ctx context.Context) (tenant.ID, error) {
	id, err := tenant.FromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("vault: %w", err)
	}
	return id, nil
}

// passphraseTenant returns ctx's tenant when the vault uses a passphrase, ErrManagedKey otherwise.
func (m *Manager) passphraseTenant(ctx context.Context) (tenant.ID, error) {
	tid, err := vaultTenant(ctx)
	if err != nil {
		return "", err
	}
	if m.passphrase == nil {
		return "", ErrManagedKey
	}
	return tid, nil
}

func (m *Manager) Init(ctx context.Context, pass string) error {
	tid, err := m.passphraseTenant(ctx)
	if err != nil {
		return err
	}
	if err := m.pm.ValidatePass(pass); err != nil {
		return err
	}

	isInit, err := m.isDatabaseInitialized(ctx)
	if err != nil {
		return err
	}

	if isInit {
		return errors2.APIError{
			Message:    "vault is already initialized",
			HTTPStatus: http.StatusConflict,
		}
	}

	err = m.dbFactory.Init()
	if err != nil {
		return err
	}
	m.logger.Infof("initialized vault")

	dbStatus := DbStatus{
		StatusName: DbStatusInit,
	}
	dbStatus.EncCheckValue, dbStatus.DecCheckValue, err = m.pm.GetEncRandValue(pass)
	if err != nil {
		return err
	}

	db := m.dbFactory.GetDbProvider()

	err = db.SetStatus(ctx, dbStatus)
	if err != nil {
		return err
	}

	m.passphrase.setPass(tid, pass)
	m.logger.Infof("unlocked vault")

	return nil
}

func (m *Manager) isDatabaseInitialized(ctx context.Context) (bool, error) {
	db := m.dbFactory.GetDbProvider()

	dbStatus, err := db.GetStatus(ctx)
	if err != nil {
		if errors.Is(err, ErrDatabaseNotInitialised) {
			return false, nil
		}
		return false, err
	}

	if dbStatus.StatusName == DbStatusInit {
		return true, nil
	}

	return false, nil
}

func (m *Manager) UnLock(ctx context.Context, pass string) error {
	tid, err := m.passphraseTenant(ctx)
	if err != nil {
		return err
	}
	if m.passphrase.passOf(tid) != "" {
		return errors2.APIError{
			Message:    "vault is already unlocked",
			HTTPStatus: http.StatusConflict,
		}
	}

	db := m.dbFactory.GetDbProvider()

	dbStatus, err := db.GetStatus(ctx)
	if err != nil {
		return err
	}
	if dbStatus.StatusName == "" || dbStatus.StatusName == DbStatusNotInit {
		return errors2.APIError{
			Message:    "vault is not yet initialized",
			HTTPStatus: http.StatusConflict,
		}
	}

	passMatch, err := m.pm.PassMatch(dbStatus, pass)
	if err != nil {
		return err
	}

	if !passMatch {
		return WrongPasswordError
	}

	m.logger.Infof("unlocked vault")

	m.passphrase.setPass(tid, pass)

	return nil
}

func (m *Manager) Lock(ctx context.Context) error {
	tid, err := m.passphraseTenant(ctx)
	if err != nil {
		return err
	}
	if m.passphrase.passOf(tid) == "" {
		return errors2.APIError{
			Message:    "vault is already locked",
			HTTPStatus: http.StatusConflict,
		}
	}

	db := m.dbFactory.GetDbProvider()
	dbStatus, err := db.GetStatus(ctx)
	if err != nil {
		return err
	}
	if dbStatus.StatusName == "" || dbStatus.StatusName == DbStatusNotInit {
		return errors2.APIError{
			Message:    "vault is not yet initialized",
			HTTPStatus: http.StatusConflict,
		}
	}

	m.logger.Infof("locked vault")
	m.passphrase.setPass(tid, "")

	return nil
}

// IsLocked reports whether the vault of ctx's tenant is locked. Without a tenant it is locked.
func (m *Manager) IsLocked(ctx context.Context) bool {
	return !m.keys.Unlocked(ctx)
}

func (m *Manager) Status(ctx context.Context) (StatusReport, error) {
	sr := StatusReport{}
	if _, err := vaultTenant(ctx); err != nil {
		return sr, err
	}

	if m.passphrase == nil {
		sr.InitStatus = DbStatusInit
		sr.LockStatus = StatusUnlocked
		if m.IsLocked(ctx) {
			sr.LockStatus = StatusLocked
		}
		sr.Managed = true
		return sr, nil
	}

	db := m.dbFactory.GetDbProvider()
	dbStatus, err := db.GetStatus(ctx)
	if err != nil && !errors.Is(err, ErrDatabaseNotInitialised) {
		return sr, err
	}

	if dbStatus.StatusName == "" {
		dbStatus.StatusName = DbStatusNotInit
	}

	sr.InitStatus = dbStatus.StatusName

	if dbStatus.StatusName == DbStatusNotInit {
		return sr, nil
	}

	if m.IsLocked(ctx) {
		sr.LockStatus = StatusLocked
	} else {
		sr.LockStatus = StatusUnlocked
	}

	return sr, nil
}

func (m *Manager) List(ctx context.Context, re *http.Request) ([]ValueKey, error) {
	err := m.checkUnlockedAndInitialized(ctx)
	if err != nil {
		return nil, err
	}

	listOptions := query.GetListOptions(re)

	err = query.ValidateListOptions(listOptions, supportedFields, supportedFields, nil, nil)
	if err != nil {
		return nil, err
	}

	db := m.dbFactory.GetDbProvider()

	return db.List(ctx, listOptions)
}

func (m *Manager) checkGroupAccess(val *StoredValue, user UserDataProvider) error {
	if val == nil || val.RequiredGroup == "" {
		return nil
	}
	userGroupMatches := false
	for _, gr := range user.GetGroups() {
		if gr != val.RequiredGroup {
			continue
		}
		userGroupMatches = true
		break
	}
	if !userGroupMatches {
		return errors2.APIError{
			Message:    "your group doesn't allow access to this value",
			HTTPStatus: http.StatusForbidden,
		}
	}

	return nil
}

func (m *Manager) GetOne(ctx context.Context, id int, user UserDataProvider) (StoredValue, bool, error) {
	err := m.checkUnlockedAndInitialized(ctx)
	if err != nil {
		return StoredValue{}, false, err
	}

	db := m.dbFactory.GetDbProvider()

	val, found, err := db.GetByID(ctx, id)
	if err != nil {
		return StoredValue{}, false, err
	}

	if !found {
		return StoredValue{}, false, nil
	}

	err = m.checkGroupAccess(&val, user)
	if err != nil {
		return StoredValue{}, false, err
	}

	key, err := m.keys.Key(ctx)
	if err != nil {
		return StoredValue{}, false, err
	}
	decryptedValue, err := decryptValue(val.Value, key)
	if err != nil {
		return StoredValue{}, false, m.managedCryptoError(err)
	}
	val.Value = string(decryptedValue)

	return val, true, nil
}

func (m *Manager) Store(ctx context.Context, existingID int64, valueToStore *InputValue, user UserDataProvider) (StoredValueID, error) {
	err := m.checkUnlockedAndInitialized(ctx)
	if err != nil {
		return StoredValueID{}, err
	}

	err = Validate(valueToStore)
	if err != nil {
		return StoredValueID{}, err
	}

	db := m.dbFactory.GetDbProvider()

	storedValue, found, err := db.FindByKeyAndClientID(ctx, valueToStore.Key, valueToStore.ClientID)
	if err != nil {
		return StoredValueID{}, err
	}

	if existingID > 0 {
		val, found2, err := db.GetByID(ctx, int(existingID))
		if err != nil {
			return StoredValueID{}, err
		}

		if !found2 {
			return StoredValueID{}, errors2.APIError{
				Message:    "cannot find entry by the provided existingID",
				HTTPStatus: http.StatusNotFound,
			}
		}

		err = m.checkGroupAccess(&val, user)
		if err != nil {
			return StoredValueID{}, err
		}
	}

	if found && (existingID == 0 || storedValue.ID != int(existingID)) {
		return StoredValueID{}, errors2.APIError{
			Message:    fmt.Sprintf("another key '%s' exists for this client '%s'", valueToStore.Key, valueToStore.ClientID),
			HTTPStatus: http.StatusConflict,
		}
	}

	key, err := m.keys.Key(ctx)
	if err != nil {
		return StoredValueID{}, err
	}
	encValue, err := encryptValue([]byte(valueToStore.Value), key)
	if err != nil {
		return StoredValueID{}, m.managedCryptoError(err)
	}

	valueToStore.Value = encValue

	res := StoredValueID{}
	res.ID, err = db.Save(ctx, user.GetUsername(), existingID, valueToStore, time.Now())
	if err != nil {
		return StoredValueID{}, err
	}

	return res, nil
}

func (m *Manager) Delete(ctx context.Context, id int, user UserDataProvider) error {
	err := m.checkUnlockedAndInitialized(ctx)
	if err != nil {
		return err
	}

	db := m.dbFactory.GetDbProvider()

	storedValue, found, err := db.GetByID(ctx, id)
	if err != nil {
		return err
	}

	if !found {
		return errors2.APIError{
			Message:    "cannot find this entry by the provided id",
			HTTPStatus: http.StatusNotFound,
		}
	}

	err = m.checkGroupAccess(&storedValue, user)
	if err != nil {
		return err
	}

	err = db.Delete(ctx, id)
	if err != nil {
		return err
	}

	return nil
}

var errVaultLocked = errors2.APIError{
	Message:    "vault is locked",
	HTTPStatus: http.StatusConflict,
}

// checkUnlocked fails when ctx has no tenant or the tenant's vault is locked.
func (m *Manager) checkUnlocked(ctx context.Context) error {
	if _, err := vaultTenant(ctx); err != nil {
		return err
	}
	if !m.keys.Unlocked(ctx) {
		return errVaultLocked
	}
	return nil
}

func (m *Manager) checkUnlockedAndInitialized(ctx context.Context) error {
	if err := m.checkUnlocked(ctx); err != nil {
		return err
	}

	if m.passphrase == nil {
		return m.checkManagedTenant(ctx)
	}

	isInit, err := m.isDatabaseInitialized(ctx)
	if err != nil {
		return err
	}

	if !isInit {
		return errors2.APIError{
			Message:    "vault is not initialized",
			HTTPStatus: http.StatusConflict,
		}
	}

	return nil
}

// openManagedDB opens the vault database of a managed vault, which is never initialised through the API.
func (m *Manager) openManagedDB() error {
	if m.dbReady.Load() {
		return nil
	}
	if err := m.dbFactory.Init(); err != nil {
		return err
	}
	m.dbReady.Store(true)
	return nil
}

// ErrPassphraseVault is returned in managed mode when the tenant's vault was set up with a passphrase: its values
// are encrypted with the passphrase key, not the key provider's.
var ErrPassphraseVault = errors2.APIError{
	Message:    "this vault was set up with a passphrase: its values must be re-encrypted for the server's key provider before it can be used",
	HTTPStatus: http.StatusConflict,
}

// errManagedCrypto is what the API caller gets when a value cannot be encrypted or decrypted with the key of a
// managed provider; the cause is logged.
var errManagedCrypto = errors2.APIError{
	Message:    "the vault value cannot be encrypted or decrypted with the server's vault key",
	HTTPStatus: http.StatusInternalServerError,
}

// checkManagedTenant opens the database of a managed vault and refuses a tenant whose vault was set up with a
// passphrase, so values encrypted under two different keys never mix.
func (m *Manager) checkManagedTenant(ctx context.Context) error {
	if err := m.openManagedDB(); err != nil {
		return err
	}
	tid, err := vaultTenant(ctx)
	if err != nil {
		return err
	}
	if _, ok := m.managedTenants.Load(tid); ok {
		return nil
	}
	st, err := m.dbFactory.GetDbProvider().GetStatus(ctx)
	if err != nil {
		return err
	}
	if st.StatusName == DbStatusInit || st.EncCheckValue != "" {
		return ErrPassphraseVault
	}
	m.managedTenants.Store(tid, struct{}{})
	return nil
}

// managedCryptoError hides the cause of a value encryption error from the API caller in managed mode. In passphrase
// mode the error is returned as before.
func (m *Manager) managedCryptoError(err error) error {
	if m.passphrase != nil {
		return err
	}
	if m.logger != nil {
		m.logger.Errorf("vault: %v", err)
	}
	return errManagedCrypto
}

func (m *Manager) Close() error {
	db := m.dbFactory.GetDbProvider()
	return db.Close()
}

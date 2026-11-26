package session

import (
	"context"
	"errors"
	"time"

	"github.com/patrickmn/go-cache"

	"github.com/pohcee/procy-server/share/tenant"
)

type APISession struct {
	TenantID     tenant.ID `json:"-" db:"tenant_id"`
	SessionID    int64     `json:"session_id" db:"session_id"`
	ExpiresAt    time.Time `json:"expires_at" db:"expires_at"`
	LastAccessAt time.Time `json:"last_access_at" db:"last_access_at"`
	Username     string    `json:"username" db:"username"`
	UserAgent    string    `json:"user_agent" db:"user_agent"`
	IPAddress    string    `json:"ip_address" db:"ip_address"`
}

// current implementation provided by go-cache
type InternalCacheProvider interface {
	Set(k string, item interface{}, d time.Duration)
	Get(k string) (item interface{}, found bool)
	Delete(k string)
	ItemCount() int
	// using `cache.Item` creates a interface dependency on go-cache but currently
	// not worth de-coupling. if alternative cache implementations are required then
	// deal with this then.
	Items() map[string]cache.Item
}

// ErrNotFound is returned (wrapped) by StorageProvider.Save when it updates a session that is no longer stored, for
// example one deleted by a logout while a request using it was still running.
var ErrNotFound = errors.New("api session not found")

type StorageProvider interface {
	Get(ctx context.Context, sessionID int64) (found bool, sessionInfo APISession, err error)
	GetAll(ctx context.Context) ([]APISession, error)
	Save(ctx context.Context, session APISession) (sessionID int64, err error)
	Delete(ctx context.Context, sessionID int64) error
	DeleteExpired(ctx context.Context) error
	Close() error

	DeleteAllByUser(ctx context.Context, username string) (err error)
	DeleteByID(ctx context.Context, username string, sessionID int64) (err error)
}

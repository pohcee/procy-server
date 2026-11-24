package session

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/patrickmn/go-cache"

	"github.com/openrport/openrport/share/tenant"
)

// Cache keeps the live API sessions in memory, keyed by tenant and session ID, in front of the storage.
// Every method is scoped to the tenant carried by its context and fails with tenant.ErrMissing when there is none.
type Cache struct {
	cache   InternalCacheProvider
	storage StorageProvider
}

func NewCache(
	ctx context.Context,
	defaultExpiration,
	cleanupInterval time.Duration,
	storage StorageProvider,
	c InternalCacheProvider,
) (*Cache, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	if c == nil {
		c = cache.New(defaultExpiration, cleanupInterval)
	}
	now := time.Now()
	// loads the sessions of the context's tenant
	validSessions, err := storage.GetAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to get api sessions from storage: %w", err)
	}

	for _, cur := range validSessions {
		cur.TenantID = tid
		c.Set(cacheKey(tid, cur.SessionID), cur, cur.ExpiresAt.Sub(now))
	}

	return &Cache{
		cache:   c,
		storage: storage,
	}, nil
}

func (p *Cache) Get(ctx context.Context, sessionID int64) (found bool, sessionInfo APISession, err error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return false, APISession{}, err
	}
	found, sessionInfo, err = p.getFromCache(tid, sessionID)
	if err != nil || found {
		return found, sessionInfo, err
	}
	return p.getFromStorage(ctx, tid, sessionID)
}

// getFromStorage reads a session missing from the cache (one this cache never loaded, e.g. of a tenant other than
// the one it was built for, or one saved by another process) and caches it until it expires. An expired session is
// not returned.
func (p *Cache) getFromStorage(ctx context.Context, tid tenant.ID, sessionID int64) (found bool, sessionInfo APISession, err error) {
	found, sessionInfo, err = p.storage.Get(ctx, sessionID)
	if err != nil || !found {
		return false, APISession{}, err
	}
	ttl := time.Until(sessionInfo.ExpiresAt)
	if ttl <= 0 {
		return false, APISession{}, nil
	}
	sessionInfo.TenantID = tid
	p.cache.Set(cacheKey(tid, sessionID), sessionInfo, ttl)
	return true, sessionInfo, nil
}

func (p *Cache) Save(ctx context.Context, session APISession) (sessionID int64, err error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return -1, err
	}
	// always save to storage as we want the token last access time saved
	sessionID, err = p.storage.Save(ctx, session)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// deleted meanwhile (logout): don't bring it back into the cache
			p.cache.Delete(cacheKey(tid, session.SessionID))
		}
		return -1, err
	}

	// make sure the session id is included in the cache version.
	session.SessionID = sessionID
	session.TenantID = tid

	p.cache.Set(cacheKey(tid, sessionID), session, time.Until(session.ExpiresAt))

	return sessionID, nil
}

func (p *Cache) Delete(ctx context.Context, sessionID int64) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	if err := p.storage.Delete(ctx, sessionID); err != nil {
		return err
	}

	p.cache.Delete(cacheKey(tid, sessionID))

	return nil
}

func (p *Cache) DeleteExpired(ctx context.Context) error {
	// only delete expired token in storage as go-cache will have already expired
	// the internal cache tokens.
	return p.storage.DeleteExpired(ctx)
}

func (p *Cache) Close() error {
	return p.storage.Close()
}

func (p *Cache) getFromCache(tid tenant.ID, sessionID int64) (found bool, sessionInfo APISession, err error) {
	existingObj, found := p.cache.Get(cacheKey(tid, sessionID))
	if !found {
		return false, APISession{}, nil
	}

	existing, ok := existingObj.(APISession)
	if !ok {
		return false, APISession{}, fmt.Errorf("invalid cache entry: expected APISession, got %T", existingObj)
	}

	return true, existing, nil
}

func (p *Cache) GetAllByUser(ctx context.Context, username string) (sessions []APISession, err error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	// the storage holds every live session, also those this cache never loaded (of another tenant than the one it
	// was built for, or saved by another process); cached entries are more up to date, so they win
	byID := map[int64]APISession{}
	stored, err := p.storage.GetAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range stored {
		if s.Username == username {
			s.TenantID = tid
			byID[s.SessionID] = s
		}
	}
	for _, item := range p.cache.Items() {
		session, ok := item.Object.(APISession)
		if !ok {
			return nil, fmt.Errorf("invalid cache entry: expected *APISession, got %T", item.Object)
		}
		if session.TenantID == tid && session.Username == username {
			byID[session.SessionID] = session
		}
	}
	sessions = make([]APISession, 0, len(byID))
	for _, s := range byID {
		sessions = append(sessions, s)
	}

	// sort into oldest accessed first
	sort.SliceStable(sessions, func(a, b int) bool {
		return sessions[a].LastAccessAt.Before(sessions[b].LastAccessAt)
	})
	return sessions, nil
}

func (p *Cache) DeleteByID(ctx context.Context, username string, sessionID int64) (err error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	err = p.deleteUserSessionsFromCache(tid, username, sessionID)
	if err != nil {
		return err
	}
	err = p.deleteUserSessionFromStorage(ctx, username, sessionID)
	if err != nil {
		return err
	}
	return nil
}

func (p *Cache) deleteUserSessionsFromCache(tid tenant.ID, username string, sessionID int64) (err error) {
	if sessionID != -1 {
		p.cache.Delete(cacheKey(tid, sessionID))
		return
	}

	// Items() returns a copy of the underlying unexpired cache items and Delete
	// won't error if item not found. this should be thread safe.
	for _, item := range p.cache.Items() {
		session, ok := item.Object.(APISession)
		if !ok {
			return fmt.Errorf("invalid cache entry: expected *APISession, got %T", item.Object)
		}
		if session.TenantID == tid && session.Username == username {
			p.cache.Delete(cacheKey(tid, session.SessionID))
		}
	}

	return nil
}

func (p *Cache) deleteUserSessionFromStorage(ctx context.Context, username string, sessionID int64) (err error) {
	err = p.storage.DeleteByID(ctx, username, sessionID)
	if err != nil {
		return fmt.Errorf("unable to delete session from storage: %w", err)
	}
	return nil
}

func (p *Cache) DeleteAllByUser(ctx context.Context, username string) (err error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	err = p.deleteUserSessionsFromCache(tid, username, -1 /* delete all sessions for user */)
	if err != nil {
		return err
	}
	err = p.deleteUserSessionsFromStorage(ctx, username)
	if err != nil {
		return err
	}
	return nil
}

func (p *Cache) deleteUserSessionsFromStorage(ctx context.Context, username string) (err error) {
	err = p.storage.DeleteAllByUser(ctx, username)
	if err != nil {
		return fmt.Errorf("unable to delete sessions from storage: %w", err)
	}
	return nil
}

// cacheKey is the in-memory key of a session: its tenant and ID.
func cacheKey(tid tenant.ID, sessionID int64) string {
	return string(tid) + "/" + strconv.FormatInt(sessionID, 10)
}

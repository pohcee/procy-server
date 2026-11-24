package session

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/db/migration/api_sessions"
	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/share/tenant"
)

// SqliteProvider stores API sessions. Every method is scoped to the tenant carried by its context
// and fails with tenant.ErrMissing when there is none.
type SqliteProvider struct {
	db *sqlx.DB
}

func NewSqliteProvider(dbPath string, dataSourceOptions sqlite.DataSourceOptions) (*SqliteProvider, error) {
	db, err := sqlite.New(dbPath, api_sessions.AssetNames(), api_sessions.Asset, dataSourceOptions)
	if err != nil {
		return nil, fmt.Errorf("unable to create api session DB instance: %w", err)
	}

	return &SqliteProvider{db: db}, nil
}

func tenantOf(ctx context.Context) (tenant.ID, error) {
	id, err := tenant.FromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("api sessions: %w", err)
	}
	return id, nil
}

func (p *SqliteProvider) GetAll(ctx context.Context) ([]APISession, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return nil, err
	}
	var result []APISession
	err = p.db.SelectContext(
		ctx, &result,
		"SELECT * FROM api_sessions WHERE tenant_id = ? AND DATETIME(expires_at) >= DATETIME(?)",
		tid,
		time.Now(),
	)
	if err != nil {
		return result, fmt.Errorf("unable to get api sessions from DB: %w", err)
	}

	return result, nil
}

func (p *SqliteProvider) Get(ctx context.Context, sessionID int64) (found bool, sessionInfo APISession, err error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return false, APISession{}, err
	}
	err = p.db.GetContext(ctx,
		&sessionInfo,
		"SELECT * FROM api_sessions WHERE tenant_id = ? AND session_id = ?",
		tid,
		sessionID,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, APISession{}, nil
		}

		return false, APISession{}, fmt.Errorf("unable to get api session from DB by token: %w", err)
	}

	return true, sessionInfo, nil
}

func (p *SqliteProvider) Save(ctx context.Context, session APISession) (sessionID int64, err error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return -1, err
	}
	session.TenantID = tid
	if session.SessionID == 0 {
		sessionID, err = p.add(ctx, session)
	} else {
		sessionID, err = p.update(ctx, session)
	}

	if err != nil {
		return -1, err
	}

	return sessionID, nil
}

func (p *SqliteProvider) add(ctx context.Context, session APISession) (sessionID int64, err error) {
	result, err := p.db.NamedExecContext(
		ctx,
		"INSERT INTO"+
			" api_sessions (tenant_id, expires_at, username, last_access_at, user_agent, ip_address)"+
			" VALUES (:tenant_id, :expires_at, :username, :last_access_at, :user_agent, :ip_address)",
		session,
	)
	if err != nil {
		return 0, fmt.Errorf("unable to create api session: %w", err)
	}

	sessionID, err = result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("unable to get api session ID for save: %w", err)
	}

	return sessionID, nil
}

func (p *SqliteProvider) update(ctx context.Context, session APISession) (sessionID int64, err error) {
	_, err = p.db.NamedExecContext(
		ctx,
		"UPDATE api_sessions"+
			"  SET expires_at=:expires_at,username=:username,"+
			"   last_access_at=:last_access_at, user_agent=:user_agent, ip_address=:ip_address"+
			"  WHERE tenant_id=:tenant_id AND session_id=:session_id",
		session,
	)
	if err != nil {
		return 0, fmt.Errorf("unable to update api session: %w", err)
	}

	sessionID = session.SessionID
	return sessionID, nil
}

func (p *SqliteProvider) Delete(ctx context.Context, sessionID int64) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	_, err = p.db.ExecContext(
		ctx,
		"DELETE FROM api_sessions WHERE tenant_id = ? AND session_id = ?",
		tid,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("unable to delete api session by token: %w", err)
	}

	return nil
}

func (p *SqliteProvider) DeleteExpired(ctx context.Context) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	_, err = p.db.ExecContext(
		ctx,
		"DELETE FROM api_sessions WHERE tenant_id = ? AND DATETIME(expires_at) <= DATETIME(?)",
		tid,
		time.Now(),
	)
	if err != nil {
		return fmt.Errorf("unable to delete expired api sessions: %w", err)
	}

	return nil
}

func (p *SqliteProvider) Close() error {
	return p.db.Close()
}

func (p *SqliteProvider) DeleteAllByUser(ctx context.Context, username string) (err error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	_, err = p.db.ExecContext(
		ctx,
		"DELETE FROM api_sessions WHERE tenant_id = ? AND username = ?",
		tid,
		username,
	)
	if err != nil {
		return err
	}

	return nil
}

func (p *SqliteProvider) DeleteByID(ctx context.Context, username string, sessionID int64) (err error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	_, err = p.db.ExecContext(
		ctx,
		"DELETE FROM api_sessions WHERE tenant_id = ? AND username = ? AND session_id = ?",
		tid,
		username,
		sessionID,
	)
	if err != nil {
		return err
	}

	return nil
}

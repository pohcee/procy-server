package vault

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/openrport/openrport/share/logger"
	"github.com/openrport/openrport/share/query"

	errors2 "github.com/openrport/openrport/server/api/errors"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/share/tenant"
)

var ErrDatabaseNotInitialised = errors2.APIError{
	Err:        errors.New("vault is not initialized yet"),
	HTTPStatus: http.StatusConflict,
}
var DataSourceOptions = sqlite.DataSourceOptions{WALEnabled: false}

// SqliteProvider stores the vault status and values. Every method is scoped to the tenant carried by its context
// and fails with tenant.ErrMissing when there is none.
type SqliteProvider struct {
	db        *sqlx.DB
	logger    *logger.Logger
	converter *query.SQLConverter
}

func tenantOf(ctx context.Context) (tenant.ID, error) {
	id, err := tenant.FromContext(ctx)
	if err != nil {
		return "", fmt.Errorf("vault: %w", err)
	}
	return id, nil
}

const statusColumns = "`id`, `db_status`, `enc_check`, `dec_check`"
const valueColumns = "`id`, `client_id`, `required_group`, `created_at`, `created_by`, `updated_at`, `updated_by`, `key`, `value`, `type`"

var _ DbProvider = (*SqliteProvider)(nil)

// NewSqliteProvider stores the vault in db, an opened and migrated vault database.
func NewSqliteProvider(db *sqlx.DB, logger *logger.Logger) *SqliteProvider {
	return &SqliteProvider{
		logger:    logger,
		db:        db,
		converter: query.NewSQLConverter(db.DriverName()),
	}
}

func (p *SqliteProvider) Close() error {
	if p.db != nil {
		return p.db.Close()
	}

	return nil
}

func (p *SqliteProvider) GetStatus(ctx context.Context) (DbStatus, error) {
	res := DbStatus{}
	tid, err := tenantOf(ctx)
	if err != nil {
		return res, err
	}
	err = p.db.GetContext(ctx, &res, "SELECT "+statusColumns+" FROM `status` WHERE `tenant_id` = ? LIMIT 1", tid)
	if err != nil {
		if err == sql.ErrNoRows {
			return res, nil
		}
		return res, err
	}

	return res, nil
}

func (p *SqliteProvider) SetStatus(ctx context.Context, newStatus DbStatus) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	tx, err := p.db.Beginx()
	if err != nil {
		return err
	}

	var idToUpdate int
	err = tx.GetContext(ctx, &idToUpdate, "SELECT id FROM `status` WHERE `tenant_id` = ? LIMIT 1", tid)
	if err != nil {
		if err == sql.ErrNoRows {
			idToUpdate = 0
		} else {
			p.handleRollback(tx)
			return err
		}
	}

	if idToUpdate == 0 {
		_, err = tx.ExecContext(
			ctx,
			"INSERT INTO `status` (`tenant_id`, `db_status`, `enc_check`, `dec_check`) VALUES (?, ?, ?, ?)",
			tid,
			newStatus.StatusName,
			newStatus.EncCheckValue,
			newStatus.DecCheckValue,
		)

		if err != nil {
			p.handleRollback(tx)
			return err
		}
	} else {
		q := "UPDATE `status` SET db_status=?, enc_check = ?, dec_check = ? WHERE tenant_id = ? AND id = ?"
		params := []interface{}{
			newStatus.StatusName,
			newStatus.EncCheckValue,
			newStatus.DecCheckValue,
			tid,
			idToUpdate,
		}
		_, err = tx.ExecContext(ctx, q, params...)
		if err != nil {
			p.handleRollback(tx)
			return err
		}
	}

	err = tx.Commit()
	if err != nil {
		return err
	}

	return nil
}

func (p *SqliteProvider) GetByID(ctx context.Context, id int) (val StoredValue, found bool, err error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return val, false, err
	}
	err = p.db.GetContext(ctx, &val, "SELECT "+valueColumns+" FROM `values` WHERE `tenant_id` = ? AND `id` = ? LIMIT 1", tid, id)
	if err != nil {
		if err == sql.ErrNoRows {
			return val, false, nil
		}

		return val, false, err
	}

	return val, true, nil
}

func (p *SqliteProvider) List(ctx context.Context, lo *query.ListOptions) ([]ValueKey, error) {
	values := []ValueKey{}
	tid, err := tenantOf(ctx)
	if err != nil {
		return values, err
	}

	q := "SELECT `id`, `client_id`, `created_by`, `created_at`, `key` FROM `values` WHERE `tenant_id` = ?"

	q, params := p.converter.ConvertListOptionsToQuery(lo, q)
	params = append([]interface{}{tid}, params...)

	err = p.db.SelectContext(ctx, &values, q, params...)
	if err != nil {
		return values, err
	}

	return values, nil
}

func (p *SqliteProvider) FindByKeyAndClientID(ctx context.Context, key, clientID string) (val StoredValue, found bool, err error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return val, false, err
	}
	err = p.db.GetContext(ctx, &val, "SELECT "+valueColumns+" FROM `values` WHERE `tenant_id` = ? AND `key` = ? and `client_id` = ? LIMIT 1", tid, key, clientID)
	if err != nil {
		if err == sql.ErrNoRows {
			return val, false, nil
		}

		return val, false, err
	}

	return val, true, nil
}

func (p *SqliteProvider) Save(ctx context.Context, user string, idToUpdate int64, val *InputValue, nowDate time.Time) (int64, error) {
	tid, err := tenantOf(ctx)
	if err != nil {
		return 0, err
	}
	if idToUpdate == 0 {
		res, err := p.db.ExecContext(
			ctx,
			"INSERT INTO `values` (`tenant_id`, `client_id`, `required_group`, `created_at`, `created_by`, `updated_at`, `updated_by`, `key`, `value`, `type`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			tid,
			val.ClientID,
			val.RequiredGroup,
			nowDate.Format(time.RFC3339),
			user,
			nowDate.Format(time.RFC3339),
			user,
			val.Key,
			val.Value,
			val.Type,
		)

		if err != nil {
			return 0, err
		}
		idToUpdate, err = res.LastInsertId()
		if err != nil {
			return 0, err
		}
	} else {
		q := "UPDATE `values` SET `client_id` = ?, `required_group` = ?, `updated_at` = ?, `updated_by` = ?, `key` = ?, `value` = ?, `type` = ? WHERE tenant_id = ? AND id = ?"
		params := []interface{}{
			val.ClientID,
			val.RequiredGroup,
			nowDate.Format(time.RFC3339),
			user,
			val.Key,
			val.Value,
			val.Type,
			tid,
			idToUpdate,
		}
		_, err := p.db.ExecContext(ctx, q, params...)
		if err != nil {
			return 0, err
		}
	}

	return idToUpdate, nil
}

func (p *SqliteProvider) Delete(ctx context.Context, id int) error {
	tid, err := tenantOf(ctx)
	if err != nil {
		return err
	}
	res, err := p.db.ExecContext(ctx, "DELETE FROM `values` WHERE `tenant_id` = ? AND `id` = ?", tid, id)

	if err != nil {
		return err
	}

	affectedRows, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if affectedRows == 0 {
		return fmt.Errorf("cannot find entry by id %d", id)
	}

	return nil
}

func (p *SqliteProvider) handleRollback(tx *sqlx.Tx) {
	err := tx.Rollback()
	if err != nil {
		p.logger.Errorf("Failed to rollback transaction: %v", err)
	}
}

type NotInitDbProvider struct{}

func (nidp *NotInitDbProvider) Init(_ context.Context) error {
	return ErrDatabaseNotInitialised
}

func (nidp *NotInitDbProvider) GetStatus(_ context.Context) (DbStatus, error) {
	return DbStatus{}, ErrDatabaseNotInitialised
}

func (nidp *NotInitDbProvider) SetStatus(_ context.Context, _ DbStatus) error {
	return ErrDatabaseNotInitialised
}

func (nidp *NotInitDbProvider) GetByID(_ context.Context, _ int) (val StoredValue, found bool, err error) {
	err = ErrDatabaseNotInitialised
	return
}

func (nidp *NotInitDbProvider) List(_ context.Context, _ *query.ListOptions) ([]ValueKey, error) {
	return nil, ErrDatabaseNotInitialised
}

func (nidp *NotInitDbProvider) FindByKeyAndClientID(_ context.Context, _, _ string) (val StoredValue, found bool, err error) {
	err = ErrDatabaseNotInitialised
	return
}

func (nidp *NotInitDbProvider) Save(_ context.Context, _ string, _ int64, _ *InputValue, _ time.Time) (int64, error) {
	return 0, ErrDatabaseNotInitialised
}

func (nidp *NotInitDbProvider) Delete(_ context.Context, _ int) error {
	return ErrDatabaseNotInitialised
}

func (nidp *NotInitDbProvider) Close() error {
	return nil
}

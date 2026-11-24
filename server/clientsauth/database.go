package clientsauth

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/mattn/go-sqlite3"

	"github.com/openrport/openrport/share/query"
	"github.com/openrport/openrport/share/tenant"

	"github.com/openrport/openrport/share/enums"
)

const mysqlDuplicateEntryErrorCode = 1062

type DatabaseProvider struct {
	db        *sqlx.DB
	tableName string
	converter *query.SQLConverter
}

var _ Provider = &DatabaseProvider{}

func NewDatabaseProvider(DB *sqlx.DB, tableName string) *DatabaseProvider {
	return &DatabaseProvider{
		db:        DB,
		tableName: tableName,
		converter: query.NewSQLConverter(DB.DriverName()),
	}
}

func (c *DatabaseProvider) GetFiltered(ctx context.Context, filter *query.ListOptions) ([]*ClientAuth, int, error) {
	if err := tenant.RequireDefault(ctx); err != nil {
		return nil, 0, err
	}

	filter.Sorts = append(filter.Sorts, query.SortOption{Column: "id", IsASC: true})
	rQuery, rParams := c.converter.ConvertListOptionsToQuery(filter, fmt.Sprintf("SELECT id,password FROM %s", c.tableName))
	filter.Pagination = nil
	filter.Sorts = nil
	cQuery, cParams := c.converter.ConvertListOptionsToQuery(filter, fmt.Sprintf("SELECT COUNT(id) FROM %s", c.tableName))
	var count = 0
	if err := c.db.GetContext(ctx, &count, cQuery, cParams...); err != nil {
		return nil, 0, err
	}
	var result = []*ClientAuth{}
	err := c.db.SelectContext(ctx, &result, rQuery, rParams...)
	return result, count, err
}

func (c *DatabaseProvider) Get(ctx context.Context, id string) (*ClientAuth, error) {
	if err := tenant.RequireDefault(ctx); err != nil {
		return nil, err
	}
	result := &ClientAuth{}
	err := c.db.GetContext(ctx, result, fmt.Sprintf("SELECT id, password FROM %s WHERE id = ?", c.tableName), id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return result, err
}

func (c *DatabaseProvider) Add(ctx context.Context, client *ClientAuth) (bool, error) {
	if err := tenant.RequireDefault(ctx); err != nil {
		return false, err
	}
	_, err := c.db.NamedExecContext(ctx, fmt.Sprintf("INSERT INTO %s (id, password) VALUES (:id, :password)", c.tableName), client)
	if err != nil {
		// Check for client already exists error
		switch typeErr := err.(type) {
		case sqlite3.Error:
			if typeErr.Code == sqlite3.ErrConstraint {
				return false, nil
			}
		case *mysql.MySQLError:
			if typeErr.Number == mysqlDuplicateEntryErrorCode {
				return false, nil
			}
		}
		return false, err
	}
	return true, nil
}

func (c *DatabaseProvider) Delete(ctx context.Context, id string) error {
	if err := tenant.RequireDefault(ctx); err != nil {
		return err
	}
	_, err := c.db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE id = ?", c.tableName), id)
	return err
}

func (c *DatabaseProvider) IsWriteable() bool {
	return true
}

func (c *DatabaseProvider) Source() enums.ProviderSource {
	return enums.ProviderSourceDB
}

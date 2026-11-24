package sqlite_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/db/sqlite"
	"github.com/openrport/openrport/server/notifications"
	repo "github.com/openrport/openrport/server/notifications/repository/sqlite"
	"github.com/openrport/openrport/server/repotest"
)

var _ repotest.NotificationsCleanable = repo.Cleanable(nil)

func TestRepositoryContract(t *testing.T) {
	repotest.Notifications(t, func(t *testing.T) notifications.Repository {
		db, err := sqlite.New(":memory:", repo.Migrations, sqlite.DataSourceOptions{})
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		return repo.NewRepository(db, testLog)
	})
}

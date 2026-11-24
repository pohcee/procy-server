package postgres_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/openrport/openrport/server/storage/postgres"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
)

func TestCheckAppRole(t *testing.T) {
	d := pgtest.New(t)
	assert.NoError(t, postgres.CheckAppRole(context.Background(), d.App))
	assert.ErrorContains(t, postgres.CheckAppRole(context.Background(), d.Owner), "row-level security")
}

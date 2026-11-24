package clientsauth

import (
	"testing"

	"github.com/openrport/openrport/share/query"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/share/enums"
)

func TestDatabaseProvider(t *testing.T) {
	db, err := sqlx.Connect("sqlite3", ":memory:")
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec("CREATE TABLE clients (id TEXT PRIMARY KEY, password TEXT)")
	require.NoError(t, err)
	c := &ClientAuth{ID: "test-client", Password: "test-password"}

	p := NewDatabaseProvider(db, "clients")
	assert.Equal(t, enums.ProviderSourceDB, p.Source())

	// initial empty
	filter := &query.ListOptions{
		Pagination: query.NewPagination(5, 0),
	}
	clients, _, err := p.GetFiltered(defaultCtx, filter)
	require.NoError(t, err)
	assert.ElementsMatch(t, []*ClientAuth{}, clients)

	// add new client
	added, err := p.Add(defaultCtx, c)
	require.NoError(t, err)
	assert.True(t, added)

	// should contain client
	clients, _, err = p.GetFiltered(defaultCtx, filter)
	require.NoError(t, err)
	assert.ElementsMatch(t, []*ClientAuth{c}, clients)

	client, err := p.Get(defaultCtx, c.ID)
	require.NoError(t, err)
	assert.Equal(t, c, client)

	// add existing client
	added, err = p.Add(defaultCtx, c)
	require.NoError(t, err)
	assert.False(t, added)

	// delete client
	err = p.Delete(defaultCtx, c.ID)
	require.NoError(t, err)

	// final empty
	clients, _, err = p.GetFiltered(defaultCtx, filter)
	require.NoError(t, err)
	assert.ElementsMatch(t, []*ClientAuth{}, clients)
}

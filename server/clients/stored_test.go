package clients

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pohcee/procy-server/server/clients/clientdata"
)

func TestStoredClientMatchesSQLiteRow(t *testing.T) {
	p := NewFakeClientProvider(t, nil)
	defer p.Close()
	at := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	connected := New(t).ID("c1").Logger(testLog).Build()
	disconnected := New(t).ID("c2").Logger(testLog).Build()
	disconnected.SetDisconnectedAt(&at)

	for _, c := range []*clientSqliteCase{{connected, false}, {disconnected, true}} {
		require.NoError(t, p.Save(defaultCtx, c.client))
		var row struct {
			ClientAuthID string `db:"client_auth_id"`
			Details      string `db:"details"`
		}
		require.NoError(t, p.db.Get(&row, "SELECT client_auth_id, details FROM clients WHERE id = ?", c.client.GetID()))

		s, err := EncodeStoredClient(c.client)
		require.NoError(t, err)
		assert.Equal(t, c.client.GetID(), s.ID)
		assert.Equal(t, row.ClientAuthID, s.ClientAuthID)
		assert.Equal(t, row.Details, s.Details)
		if c.disconnected {
			require.NotNil(t, s.DisconnectedAt)
			assert.True(t, at.Equal(*s.DisconnectedAt))
		} else {
			assert.Nil(t, s.DisconnectedAt)
		}

		want, err := p.get(defaultCtx, c.client.GetID(), testLog)
		require.NoError(t, err)
		got, err := DecodeStoredClient(s, testLog)
		require.NoError(t, err)
		assert.EqualValues(t, want, got)
	}
}

type clientSqliteCase struct {
	client       *clientdata.Client
	disconnected bool
}

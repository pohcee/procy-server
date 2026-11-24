package notifications_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openrport/openrport/server/repotest"

	"github.com/openrport/openrport/server/storage/postgres/notifications"
	"github.com/openrport/openrport/server/storage/postgres/pgtest"
)

// Postgres text rejects NUL bytes and invalid UTF-8, which SQLite stores; such output must still be logged.
func TestSavesOutputPostgresTextRejects(t *testing.T) {
	d := pgtest.New(t, repotest.TenantA)
	s := notifications.New(d.App)
	ctx := repotest.Ctx(repotest.TenantA)

	n := notification()
	n.Data.Subject = "sub\x00ject"
	n.Data.Content = "body\x00"
	// a multi-byte rune across the stored limit, a NUL and an invalid byte
	out := strings.Repeat("a", notifications.MaxOutAndErrorSize-1) + "é" + "tail"
	require.NoError(t, s.SetError(ctx, n, out, "bad \x00 \xff err"))

	got, found, err := s.Details(ctx, n.ID.ID())
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, utf8.ValidString(got.Out))
	assert.LessOrEqual(t, len(got.Out), notifications.MaxOutAndErrorSize)
	assert.True(t, strings.HasPrefix(got.Out, strings.Repeat("a", notifications.MaxOutAndErrorSize-1)))
	assert.NotContains(t, got.Err, "\x00")
	assert.Contains(t, got.Err, "bad")
	assert.Equal(t, "subject", got.Data.Subject)
}

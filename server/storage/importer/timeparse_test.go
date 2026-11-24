package importer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSQLiteTime(t *testing.T) {
	want := time.Date(2024, 3, 5, 14, 7, 9, 0, time.UTC)
	wantNano := time.Date(2024, 3, 5, 14, 7, 9, 123456789, time.UTC)
	for _, c := range []struct {
		in   string
		want time.Time
	}{
		{"2024-03-05 14:07:09", want},                                   // CURRENT_TIMESTAMP, monitoring layout
		{"2024-03-05T14:07:09Z", want},                                  // RFC 3339 (vault)
		{"2024-03-05T16:07:09+02:00", want},                             // RFC 3339 with an offset
		{"2024-03-05 14:07:09.123456789+00:00", wantNano},               // go-sqlite3 writing a time.Time
		{"2024-03-05 16:07:09.123456789+02:00", wantNano},               // same, local zone
		{"2024-03-05T14:07:09.123456789Z", wantNano},                    // RFC 3339 nano
		{"2024-03-05 14:07:09.123456789", wantNano},                     // no zone: UTC
		{"2024-03-05 14:07:09.123456789 +0000 UTC", wantNano},           // time.Time.String()
		{"2024-03-05 16:07:09.123456789 +0200 CEST m=+0.001", wantNano}, // with a monotonic reading
		{"2024-03-05 14:07", want.Add(-9 * time.Second)},
		{"2024-03-05", time.Date(2024, 3, 5, 0, 0, 0, 0, time.UTC)},
		{"1709647629", want},            // unix seconds
		{"1709647629000", want},         // unix milliseconds
		{" 2024-03-05 14:07:09 ", want}, // stray spaces
	} {
		got, err := parseSQLiteTime(c.in)
		require.NoError(t, err, c.in)
		assert.True(t, c.want.Equal(got), "%s: got %v, want %v", c.in, got, c.want)
		assert.Equal(t, time.UTC, got.Location(), c.in)
	}
	for _, bad := range []string{"", "yesterday", "2024-13-45 99:00:00"} {
		_, err := parseSQLiteTime(bad)
		assert.Error(t, err, bad)
	}
}

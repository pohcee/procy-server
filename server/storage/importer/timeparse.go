package importer

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// sqliteTimeLayouts are the forms in which the standalone server's SQLite files hold times: go-sqlite3 writes a bound
// time.Time with the first layout; CURRENT_TIMESTAMP and the monitoring store write "2006-01-02 15:04:05" (UTC); the
// vault writes RFC 3339; a time formatted with fmt ends up as time.Time.String(). A time without a zone is UTC, as
// SQLite and go-sqlite3 take it.
var sqliteTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999Z07:00",
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05.999999999 -0700",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04",
	"2006-01-02T15:04",
	"2006-01-02",
}

// parseSQLiteTime parses a time read as text from a SQLite file into UTC. Integers are Unix times in seconds, or in
// milliseconds when too large for seconds (go-sqlite3's rule).
func parseSQLiteTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, " m="); i >= 0 { // the monotonic clock reading of time.Time.String()
		s = s[:i]
	}
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time")
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e12 || n < -1e12 {
			return time.UnixMilli(n).UTC(), nil
		}
		return time.Unix(n, 0).UTC(), nil
	}
	for _, layout := range sqliteTimeLayouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time %q", s)
}

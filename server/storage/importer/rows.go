package importer

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/share/tenant"
)

// kind is how a source column is read and handed to Postgres.
type kind int

const (
	kText  kind = iota // text; NULL becomes ""
	kNText             // nullable text
	kTime              // a time stored as text (or a Unix time); NULL or unparsable is an error
	kNTime             // nullable time; "" is NULL as well
	kInt               // integer
	kNInt              // nullable integer
	kFloat             // real
	kBool              // integer 0/1 → boolean
	kBytes             // raw bytes (bytea), NULL stays NULL
)

// col is one column of a source table: its SQLite expression and how to read it.
type col struct {
	src  string
	kind kind
}

func c(src string, k kind) col { return col{src: src, kind: k} }

// selectExpr reads the column so that the SQLite driver hands over what is stored: times are cast to text (a column
// declared DATE or DATETIME would otherwise be parsed by the driver, which yields the zero time for a form it
// doesn't know).
func (c col) selectExpr() string {
	ident := c.src
	if !strings.ContainsAny(ident, "( ") {
		ident = quoteIdent(ident)
	}
	switch c.kind {
	case kTime, kNTime:
		return "CAST(" + ident + " AS TEXT)"
	case kBytes:
		return "CAST(" + ident + " AS BLOB)"
	}
	return ident
}

// readRows streams the rows of a source table (in the given order) to fn as Postgres-ready values, one per column.
func readRows(d *sourceDB, table string, cols []col, orderBy string, fn func(vals []any) error) error {
	exprs := make([]string, len(cols))
	for i, c := range cols {
		exprs[i] = c.selectExpr()
	}
	q := "SELECT " + strings.Join(exprs, ", ") + " FROM " + quoteIdent(table)
	if orderBy != "" {
		q += " ORDER BY " + orderBy
	}
	rows, err := d.db.Query(q)
	if err != nil {
		return fmt.Errorf("%s %s: %w", d.name, table, err)
	}
	defer rows.Close()
	for rows.Next() {
		dest := make([]any, len(cols))
		for i, c := range cols {
			switch c.kind {
			case kInt, kNInt, kBool:
				dest[i] = new(sql.NullInt64)
			case kFloat:
				dest[i] = new(sql.NullFloat64)
			case kBytes:
				dest[i] = new([]byte)
			default:
				dest[i] = new(sql.NullString)
			}
		}
		if err := rows.Scan(dest...); err != nil {
			return fmt.Errorf("%s %s: %w", d.name, table, err)
		}
		vals := make([]any, len(cols))
		for i, c := range cols {
			v, err := convert(c.kind, dest[i])
			if err != nil {
				return fmt.Errorf("%s %s: column %s: %w", d.name, table, c.src, err)
			}
			vals[i] = v
		}
		if err := fn(vals); err != nil {
			return err
		}
	}
	return rows.Err()
}

func convert(k kind, v any) (any, error) {
	switch k {
	case kText:
		return v.(*sql.NullString).String, nil
	case kNText:
		s := v.(*sql.NullString)
		if !s.Valid {
			return nil, nil
		}
		return s.String, nil
	case kTime, kNTime:
		s := v.(*sql.NullString)
		if !s.Valid || (k == kNTime && strings.TrimSpace(s.String) == "") {
			if k == kNTime {
				return nil, nil
			}
			return nil, fmt.Errorf("a time is required, the row has none")
		}
		return parseSQLiteTime(s.String)
	case kInt:
		return v.(*sql.NullInt64).Int64, nil
	case kNInt:
		n := v.(*sql.NullInt64)
		if !n.Valid {
			return nil, nil
		}
		return n.Int64, nil
	case kBool:
		return v.(*sql.NullInt64).Int64 != 0, nil
	case kFloat:
		return v.(*sql.NullFloat64).Float64, nil
	case kBytes:
		b := *(v.(*[]byte))
		if b == nil {
			return nil, nil
		}
		return b, nil
	}
	return nil, fmt.Errorf("unknown column kind %d", k)
}

// writer writes rows into the target tenant's tables, all in one transaction.
type writer struct {
	ctx     context.Context
	tx      *sqlx.Tx
	tid     tenant.ID
	cleaned map[string]int // per table: text values made storable
}

// storable makes s fit a Postgres text column, which (unlike SQLite) refuses NUL bytes and invalid UTF-8: NULs are
// dropped and invalid bytes become U+FFFD.
func storable(s string) string {
	if strings.IndexByte(s, 0) < 0 && utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "�")
}

// maxParams keeps a statement below Postgres's 65535 bind parameters.
const maxParams = 60000

// insert writes rows (each a value per column of cols) with tenant_id first, followed by suffix (an ON CONFLICT
// clause), and returns how many rows were written.
func (w *writer) insert(table string, cols []string, rows [][]any, suffix string) (int, error) {
	added := 0
	per := len(cols) + 1
	chunk := maxParams / per
	if chunk > 500 {
		chunk = 500
	}
	for start := 0; start < len(rows); start += chunk {
		end := start + chunk
		if end > len(rows) {
			end = len(rows)
		}
		var b strings.Builder
		b.WriteString("INSERT INTO " + table + " (tenant_id, " + strings.Join(cols, ", ") + ") VALUES ")
		args := make([]any, 0, (end-start)*per)
		for i, r := range rows[start:end] {
			if len(r) != len(cols) {
				return added, fmt.Errorf("%s: %d values for %d columns", table, len(r), len(cols))
			}
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("(")
			args = append(args, string(w.tid))
			b.WriteString("$" + strconv.Itoa(len(args)))
			for _, v := range r {
				if s, ok := v.(string); ok {
					if clean := storable(s); clean != s {
						w.cleaned[table]++
						v = clean
					}
				}
				args = append(args, v)
				b.WriteString(", $" + strconv.Itoa(len(args)))
			}
			b.WriteString(")")
		}
		b.WriteString(" " + suffix)
		res, err := w.tx.ExecContext(w.ctx, b.String(), args...)
		if err != nil {
			return added, fmt.Errorf("write %s: %w", table, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return added, err
		}
		added += int(n)
	}
	return added, nil
}

// insertOne writes one row unless it conflicts with an existing one and reports whether it was written.
func (w *writer) insertOne(table string, cols []string, vals []any) (bool, error) {
	n, err := w.insert(table, cols, [][]any{vals}, onConflictNothing)
	return n == 1, err
}

// exists reports whether the query (with tenant_id = $1 and args from $2) finds a row.
func (w *writer) exists(q string, args ...any) (bool, error) {
	var ok bool
	err := w.tx.GetContext(w.ctx, &ok, "SELECT EXISTS ("+q+")", append([]any{string(w.tid)}, args...)...)
	return ok, err
}

// batch collects rows and writes them in chunks, counting into an entity.
type batch struct {
	w       *writer
	table   string
	cols    []string
	suffix  string
	count   *EntityCount
	pending [][]any
}

func (w *writer) batch(table string, cols []string, suffix string, count *EntityCount) *batch {
	return &batch{w: w, table: table, cols: cols, suffix: suffix, count: count}
}

func (b *batch) add(vals []any) error {
	b.pending = append(b.pending, vals)
	if len(b.pending) >= 500 {
		return b.flush()
	}
	return nil
}

// flush writes the pending rows; those not written were already present.
func (b *batch) flush() error {
	if len(b.pending) == 0 {
		return nil
	}
	n, err := b.w.insert(b.table, b.cols, b.pending, b.suffix)
	if err != nil {
		return err
	}
	b.count.Added += n
	b.count.Present += len(b.pending) - n
	b.pending = b.pending[:0]
	return nil
}

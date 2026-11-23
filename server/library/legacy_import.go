package library

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/openrport/openrport/share/random"
)

const legacyImportKey = "legacy_import"

type legacyRow struct {
	ID          string    `db:"id"`
	Name        string    `db:"name"`
	CreatedAt   time.Time `db:"created_at"`
	CreatedBy   string    `db:"created_by"`
	UpdatedAt   time.Time `db:"updated_at"`
	UpdatedBy   string    `db:"updated_by"`
	Interpreter *string   `db:"interpreter"`
	IsSudo      *bool     `db:"is_sudo"`
	Cwd         *string   `db:"cwd"`
	Code        string    `db:"code"`
	Tags        string    `db:"tags"`
	TimeoutSec  int       `db:"timeout_sec"`
}

func (r legacyRow) tags() []string {
	var tags []string
	if err := json.Unmarshal([]byte(r.Tags), &tags); err != nil {
		return nil
	}
	return tags
}

func str(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ImportLegacy copies the rows of the old scripts and commands tables into
// entries, once. The old tables are left untouched. Rows are normalized but not
// validated, so nothing that worked before is dropped. Everything, including
// the legacy_import marker, is written in one transaction. It returns the
// number of entries created.
func ImportLegacy(ctx context.Context, db *sqlx.DB, s *Store) (int, error) {
	if _, done, err := s.Meta(ctx, legacyImportKey); err != nil || done {
		return 0, err
	}

	var scripts, commands []legacyRow
	if err := db.SelectContext(ctx, &scripts,
		"SELECT `id`, `name`, `created_at`, `created_by`, `updated_at`, `updated_by`, `interpreter`, `is_sudo`, `cwd`, `script` AS `code`, `tags`, `timeout_sec` FROM `scripts` ORDER BY `name`"); err != nil {
		return 0, fmt.Errorf("read legacy scripts: %w", err)
	}
	if err := db.SelectContext(ctx, &commands,
		"SELECT `id`, `name`, `created_at`, `created_by`, `updated_at`, `updated_by`, NULL AS `interpreter`, NULL AS `is_sudo`, NULL AS `cwd`, `cmd` AS `code`, `tags`, `timeout_sec` FROM `commands` ORDER BY `name`"); err != nil {
		return 0, fmt.Errorf("read legacy commands: %w", err)
	}

	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	ids := map[string]struct{}{}
	names := map[string]struct{}{}
	count := 0
	add := func(r legacyRow, impl Implementation, osKey, message string, isCommand bool) error {
		id := r.ID
		if _, taken := ids[id]; taken {
			var err error
			if id, err = random.UUID4(); err != nil {
				return err
			}
		}
		base := strings.TrimSpace(r.Name)
		name := base
		if _, taken := names[name]; taken {
			if isCommand {
				name = base + " (command)"
			}
			for n := 2; ; n++ {
				if _, taken := names[name]; !taken {
					break
				}
				if isCommand {
					name = base + " (command " + strconv.Itoa(n) + ")"
				} else {
					name = base + " (" + strconv.Itoa(n) + ")"
				}
			}
		}
		c := Content{Name: name, Tags: r.tags(), Implementations: map[string]Implementation{osKey: impl}}
		Normalize(&c)
		e := Entry{
			ID: id, Name: c.Name, Tags: c.Tags, ActiveVersion: 1, OS: []string{osKey},
			CreatedAt: r.CreatedAt.UTC(), CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt.UTC(), UpdatedBy: r.UpdatedBy,
		}
		v := Version{
			EntryID: id, Version: 1, CreatedAt: r.CreatedAt.UTC(), CreatedBy: r.CreatedBy,
			Message: message, Changed: Changed(Content{}, c), Content: &c,
		}
		if err := insertEntry(ctx, tx, e, v); err != nil {
			return fmt.Errorf("import %q: %w", r.Name, err)
		}
		ids[id] = struct{}{}
		names[c.Name] = struct{}{}
		count++
		return nil
	}

	for _, r := range scripts {
		interpreter := str(r.Interpreter)
		osKey := OSLinux
		switch strings.ToLower(NormalizeInterpreter(interpreter)) {
		case "powershell", "cmd":
			osKey = OSWindows
		}
		impl := Implementation{
			Kind: KindScript, Code: r.Code, Interpreter: interpreter, IsSudo: r.IsSudo != nil && *r.IsSudo,
			Cwd: str(r.Cwd), TimeoutSec: r.TimeoutSec,
		}
		if err := add(r, impl, osKey, "Imported from the script library", false); err != nil {
			return 0, err
		}
	}
	for _, r := range commands {
		impl := Implementation{Kind: KindCommand, Code: r.Code, TimeoutSec: r.TimeoutSec}
		if err := add(r, impl, OSLinux, "Imported from the command library", true); err != nil {
			return 0, err
		}
	}

	if err := s.SetMeta(ctx, tx, legacyImportKey, strconv.Itoa(count)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

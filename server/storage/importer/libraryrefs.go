package importer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// libraryEntryIDField is the field of the schedule, multi-job and job documents (details) naming the library entry
// they run. The version they ran (library_version) is a number of that entry's history and is kept as it is.
const libraryEntryIDField = "library_entry_id"

// libraryRemap maps the id a built-in library entry has in the source onto the id the same built-in (same
// builtin_key) has in the target, for the built-ins the target already has under another id: the server syncs the
// built-ins into every tenant, each under ids of its own. A customer entry keeps its id and isn't in the map.
type libraryRemap struct {
	ids  map[string]string // source id → target id
	keys map[string]string // source id → builtin_key, for the report
}

func (m *libraryRemap) add(srcID, dstID, key string) {
	if m.ids == nil {
		m.ids, m.keys = map[string]string{}, map[string]string{}
	}
	m.ids[srcID], m.keys[srcID] = dstID, key
}

func (m *libraryRemap) empty() bool { return len(m.ids) == 0 }

// describe lists the mapping as "key src → dst", ordered by key.
func (m *libraryRemap) describe() string {
	out := make([]string, 0, len(m.ids))
	for src, dst := range m.ids {
		out = append(out, fmt.Sprintf("%s %s → %s", m.keys[src], src, dst))
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// rewrite returns details with its library entry id replaced by the target's id when it names a remapped built-in,
// and whether it did. Anything else (no such field, not a JSON object, a customer entry) comes back as it is. Only
// that field changes: the other values are kept byte for byte (the object's keys are written in sorted order).
func (m *libraryRemap) rewrite(details string) (string, bool) {
	if m.empty() || !strings.Contains(details, libraryEntryIDField) {
		return details, false
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(details), &doc); err != nil {
		// a raw NUL or invalid UTF-8 (which Postgres can't store and the insert drops) may be all that is wrong
		if clean := storable(details); clean == details || json.Unmarshal([]byte(clean), &doc) != nil {
			return details, false
		}
	}
	raw, ok := doc[libraryEntryIDField]
	if !ok {
		return details, false
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		return details, false
	}
	dst, ok := m.ids[id]
	if !ok {
		return details, false
	}
	doc[libraryEntryIDField], _ = json.Marshal(dst) // a string always marshals
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // keep the text as the source had it, < and & included
	if err := enc.Encode(doc); err != nil {
		return details, false
	}
	return strings.TrimSuffix(b.String(), "\n"), true
}

// rewriteDetails rewrites the details value (vals[i], a string) in place, counting a rewrite.
func (m *libraryRemap) rewriteDetails(vals []any, i int, n *int) {
	s, ok := vals[i].(string)
	if !ok {
		return
	}
	if out, ok := m.rewrite(s); ok {
		vals[i] = out
		*n++
	}
}

// reportRewrites tells how many source rows of an entity reference a remapped built-in (rewritten before they are
// written; rows already in the target count as well, they were imported the same way or repaired).
func (im *importer) reportRewrites(ent string, n int) {
	if n > 0 {
		im.rep.warnf("%s: %d source rows run built-in library entries the target has under other ids, imported with the target's entry ids", ent, n)
	}
}

// repairSchedules rewrites the references of schedules already in the target (ids of source schedules that
// reference a remapped built-in): an import made before references were rewritten left them pointing at the
// source's ids, which the target doesn't have (the importer never copies a built-in the target has). A schedule
// whose details no longer name a remapped source id is left alone.
func (im *importer) repairSchedules(w *writer, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	var rows []struct {
		ID      string `db:"id"`
		Details string `db:"details"`
	}
	if err := w.tx.SelectContext(w.ctx, &rows, "SELECT id, details FROM schedules WHERE tenant_id = $1 AND id = ANY($2)", string(w.tid), ids); err != nil {
		return err
	}
	repaired := 0
	for _, r := range rows {
		out, ok := im.libRemap.rewrite(r.Details)
		if !ok {
			continue
		}
		if _, err := w.tx.ExecContext(w.ctx, "UPDATE schedules SET details = $3 WHERE tenant_id = $1 AND id = $2", string(w.tid), r.ID, out); err != nil {
			return fmt.Errorf("repair schedule %s: %w", r.ID, err)
		}
		repaired++
	}
	if repaired > 0 {
		im.rep.warnf("%s: %d already imported with references to the source's built-in library entry ids, rewritten to the target's ids", entSchedules, repaired)
	}
	return nil
}

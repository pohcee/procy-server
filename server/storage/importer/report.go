package importer

import (
	"fmt"
	"io"
	"strconv"

	"github.com/pohcee/procy-server/share/tenant"
)

// Report is the outcome of an import: per entity, how many rows were read from the source and what became of them.
type Report struct {
	Tenant   tenant.ID
	DryRun   bool
	Entities []EntityCount
	Warnings []string
}

// EntityCount counts one entity. Read = Added + Present + Skipped once the entity is done: Added rows were written,
// Present rows were already in the target (same key), Skipped rows were not written (see the warnings).
type EntityCount struct {
	Name                          string
	Read, Added, Present, Skipped int
}

// Get returns the count of the named entity, or nil.
func (r *Report) Get(name string) *EntityCount {
	for i := range r.Entities {
		if r.Entities[i].Name == name {
			return &r.Entities[i]
		}
	}
	return nil
}

func (r *Report) warnf(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

// Print writes the report as an aligned table, followed by the warnings.
func (r *Report) Print(w io.Writer) {
	title := fmt.Sprintf("Import into tenant %q", r.Tenant)
	if r.DryRun {
		title += " (dry run: nothing was written)"
	}
	fmt.Fprintln(w, title)

	header := []string{"ENTITY", "READ", "ADDED", "PRESENT", "SKIPPED"}
	rows := [][]string{header}
	for _, e := range r.Entities {
		rows = append(rows, []string{e.Name, strconv.Itoa(e.Read), strconv.Itoa(e.Added), strconv.Itoa(e.Present), strconv.Itoa(e.Skipped)})
	}
	widths := make([]int, len(header))
	for _, row := range rows {
		for i, c := range row {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	for _, row := range rows {
		line := fmt.Sprintf("%-*s", widths[0], row[0])
		for i := 1; i < len(row); i++ {
			line += fmt.Sprintf("  %*s", widths[i], row[i])
		}
		fmt.Fprintln(w, line)
	}
	if len(r.Warnings) > 0 {
		fmt.Fprintln(w, "Warnings:")
		for _, m := range r.Warnings {
			fmt.Fprintf(w, "  - %s\n", m)
		}
	}
}

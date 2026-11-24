package migration

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestMigrationManifest pins the name and SHA-256 of every migration file.
// Applied migrations must never change. When adding a new migration, copy the
// printed manifest into testdata/manifest.txt.
func TestMigrationManifest(t *testing.T) {
	var lines []string
	for _, s := range All() {
		entries, err := fs.ReadDir(s.FS, ".")
		if err != nil {
			t.Fatalf("%s: %v", s.Name, err)
		}
		for _, e := range entries {
			b, err := fs.ReadFile(s.FS, e.Name())
			if err != nil {
				t.Fatalf("%s/%s: %v", s.Name, e.Name(), err)
			}
			lines = append(lines, fmt.Sprintf("%s/%s %x", s.Name, e.Name(), sha256.Sum256(b)))
		}
	}
	sort.Strings(lines)
	got := strings.Join(lines, "\n") + "\n"

	want, err := os.ReadFile("testdata/manifest.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("migration files differ from testdata/manifest.txt.\n"+
			"Never edit an applied migration; add a new one instead.\n"+
			"Expected manifest for the current files:\n%s", got)
	}
}

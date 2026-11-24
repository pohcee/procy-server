package importer

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReportPrint(t *testing.T) {
	r := &Report{
		Tenant: "acme",
		DryRun: true,
		Entities: []EntityCount{
			{Name: "clients", Read: 1200, Added: 1100, Present: 100},
			{Name: "audit log", Read: 7, Skipped: 7},
		},
		Warnings: []string{"audit log: not imported (use --with-history)"},
	}
	var b bytes.Buffer
	r.Print(&b)
	assert.Equal(t, `Import into tenant "acme" (dry run: nothing was written)
ENTITY     READ  ADDED  PRESENT  SKIPPED
clients    1200   1100      100        0
audit log     7      0        0        7
Warnings:
  - audit log: not imported (use --with-history)
`, b.String())

	r = &Report{Tenant: "default", Entities: []EntityCount{{Name: "x", Read: 1, Added: 1}}}
	b.Reset()
	r.Print(&b)
	assert.Equal(t, `Import into tenant "default"
ENTITY  READ  ADDED  PRESENT  SKIPPED
x          1      1        0        0
`, b.String())
}

func TestReportGet(t *testing.T) {
	r := &Report{Entities: []EntityCount{{Name: "a", Read: 2}, {Name: "b", Added: 1}}}
	assert.Equal(t, 2, r.Get("a").Read)
	assert.Nil(t, r.Get("c"))
}

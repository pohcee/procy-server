package clients

import (
	"embed"
	"io/fs"
)

//go:embed sql/*.sql
var files embed.FS

// FS holds the migration SQL files at its root.
var FS = mustSub(files, "sql")

func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

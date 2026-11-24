package dispatch

import (
	"path"
	"path/filepath"
	"sync"
)

// FileGrants records which server files each client connection may read over SFTP. A file is
// granted only while it is being sent to that client (see Local.Upload); the client listener's
// SFTP server serves only what Allowed reports.
type FileGrants struct {
	mu     sync.Mutex
	byConn map[any]map[string]int
}

// NewFileGrants returns an empty set of grants.
func NewFileGrants() *FileGrants {
	return &FileGrants{byConn: map[any]map[string]int{}}
}

func cleanGrantPath(p string) string {
	return path.Clean(filepath.ToSlash(p))
}

// Grant lets conn read p until the returned function is called. Grants of the same file are counted.
func (g *FileGrants) Grant(conn any, p string) (revoke func()) {
	p = cleanGrantPath(p)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byConn[conn] == nil {
		g.byConn[conn] = map[string]int{}
	}
	g.byConn[conn][p]++

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			files := g.byConn[conn]
			files[p]--
			if files[p] <= 0 {
				delete(files, p)
			}
			if len(files) == 0 {
				delete(g.byConn, conn)
			}
		})
	}
}

// Allowed reports whether conn may read p.
func (g *FileGrants) Allowed(conn any, p string) bool {
	p = cleanGrantPath(p)
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.byConn[conn][p] > 0
}

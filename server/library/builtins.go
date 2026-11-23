package library

import (
	"embed"
	"encoding/json"
	"fmt"
	"path"
	"sort"
)

//go:embed builtin
var builtinFS embed.FS

// Builtin is a library entry shipped with the server.
type Builtin struct {
	Key     string
	Content Content
}

type builtinManifest struct {
	Key             string   `json:"key"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	Tags            []string `json:"tags"`
	Implementations map[string]struct {
		File        string `json:"file"`
		Kind        Kind   `json:"kind"`
		Interpreter string `json:"interpreter"`
		IsSudo      bool   `json:"is_sudo"`
		Cwd         string `json:"cwd"`
		TimeoutSec  int    `json:"timeout_sec"`
	} `json:"implementations"`
}

// Builtins loads the embedded built-in entries, ordered by key.
func Builtins() ([]Builtin, error) {
	dirs, err := builtinFS.ReadDir("builtin")
	if err != nil {
		return nil, err
	}
	var out []Builtin
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		dir := path.Join("builtin", d.Name())
		raw, err := builtinFS.ReadFile(path.Join(dir, "manifest.json"))
		if err != nil {
			return nil, fmt.Errorf("built-in %s: %w", d.Name(), err)
		}
		var m builtinManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("built-in %s: %w", d.Name(), err)
		}
		c := Content{Name: m.Name, Description: m.Description, Tags: m.Tags, Implementations: map[string]Implementation{}}
		for os, mi := range m.Implementations {
			code, err := builtinFS.ReadFile(path.Join(dir, mi.File))
			if err != nil {
				return nil, fmt.Errorf("built-in %s: %w", m.Key, err)
			}
			c.Implementations[os] = Implementation{Kind: mi.Kind, Code: string(code), Interpreter: mi.Interpreter, IsSudo: mi.IsSudo, Cwd: mi.Cwd, TimeoutSec: mi.TimeoutSec}
		}
		Normalize(&c)
		if err := Validate(c); err != nil {
			return nil, fmt.Errorf("built-in %s: %w", m.Key, err)
		}
		out = append(out, Builtin{Key: m.Key, Content: c})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// defaultDataDir is rportd's data_dir when the config sets none.
const defaultDataDir = "/var/lib/rport"

// sourceConfig is the part of the source rportd.conf the importer reads: where the data is and where users and
// client credentials come from. Keys follow server/chconfig; unknown keys are ignored (viper, which rportd uses,
// parses the same TOML).
type sourceConfig struct {
	Server struct {
		DataDir   string `toml:"data_dir"`
		Auth      string `toml:"auth"`
		AuthFile  string `toml:"auth_file"`
		AuthTable string `toml:"auth_table"`
	} `toml:"server"`
	API struct {
		Auth                  string `toml:"auth"`
		AuthFile              string `toml:"auth_file"`
		AuthUserTable         string `toml:"auth_user_table"`
		AuthGroupTable        string `toml:"auth_group_table"`
		AuthGroupDetailsTable string `toml:"auth_group_details_table"`
		AuthHeader            string `toml:"auth_header"`
	} `toml:"api"`
	Database struct {
		Type string `toml:"db_type"`
		Name string `toml:"db_name"`
	} `toml:"database"`

	dir string // directory of the config file
}

func loadSourceConfig(path string) (*sourceConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("source config: %w", err)
	}
	var c sourceConfig
	if err := toml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("source config %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	c.dir = filepath.Dir(abs)
	return &c, nil
}

// configDataDir is the data_dir the config names.
func (c *sourceConfig) configDataDir() string {
	if c.Server.DataDir == "" {
		return defaultDataDir
	}
	return c.Server.DataDir
}

// usesAuthDB reports whether users or client credentials come from [database] tables.
func (c *sourceConfig) usesAuthDB() bool {
	return c.Server.AuthTable != "" || (c.API.AuthFile == "" && c.API.Auth == "" && c.API.AuthUserTable != "")
}

// pathResolver maps paths named in the config onto the data dir being imported. A copy of a server's data restored
// elsewhere still names the original paths in its config (/var/lib/rport/client-auth.json): anything under the
// config's data_dir is read from the imported data dir instead. Relative paths are relative to the config file.
type pathResolver struct {
	configDataDir, dataDir, configDir string
}

func (p pathResolver) resolve(name string) string {
	if name == "" {
		return ""
	}
	if !filepath.IsAbs(name) {
		return filepath.Join(p.configDir, name)
	}
	clean := filepath.Clean(name)
	if p.configDataDir != "" {
		base := filepath.Clean(p.configDataDir)
		if base != filepath.Clean(p.dataDir) && (clean == base || strings.HasPrefix(clean, base+string(filepath.Separator))) {
			rel, _ := filepath.Rel(base, clean)
			return filepath.Join(p.dataDir, rel)
		}
	}
	return clean
}

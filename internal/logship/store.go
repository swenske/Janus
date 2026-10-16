package logship

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// On a node, the configuration and the cursors are kept in a directory
// of STATE's config/ (internal/api.SecretPaths: it holds secrets).
const (
	ConfigFile = "config.json"
	CursorFile = "cursors.json"
)

// LoadConfig reads the configuration kept in dir: the empty one when
// there's none.
func LoadConfig(dir string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(dir, ConfigFile))
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	if err := c.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	return c, nil
}

// SaveConfig keeps c in dir, readable by janusd only.
func SaveConfig(dir string, c Config) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, ConfigFile), append(b, '\n'), 0o600)
}

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	StateDir string
	DataDir  string
}

func Default() (Config, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return Config{}, fmt.Errorf("find configuration directory: %w", err)
	}
	return Config{
		StateDir: filepath.Join(base, "gotor-cli"),
		DataDir:  filepath.Join(base, "gotor-cli", "downloads"),
	}, nil
}

func (c Config) Absolute() (Config, error) {
	if strings.TrimSpace(c.StateDir) == "" || strings.TrimSpace(c.DataDir) == "" {
		return c, fmt.Errorf("state and data directories must not be empty")
	}
	var err error
	c.StateDir, err = filepath.Abs(c.StateDir)
	if err != nil {
		return c, fmt.Errorf("resolve state directory: %w", err)
	}
	c.DataDir, err = filepath.Abs(c.DataDir)
	if err != nil {
		return c, fmt.Errorf("resolve data directory: %w", err)
	}
	return c, nil
}

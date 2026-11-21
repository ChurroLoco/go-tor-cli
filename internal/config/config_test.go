package config

import (
	"path/filepath"
	"testing"
)

func TestAbsolute(t *testing.T) {
	got, err := (Config{StateDir: "state", DataDir: "downloads"}).Absolute()
	if err != nil || !filepath.IsAbs(got.StateDir) || !filepath.IsAbs(got.DataDir) {
		t.Fatalf("config=%+v error=%v", got, err)
	}
}

func TestDefault(t *testing.T) {
	got, err := Default()
	if err != nil || got.StateDir == "" || got.DataDir == "" || got.StateDir == got.DataDir {
		t.Fatalf("config=%+v error=%v", got, err)
	}

}

func TestAbsoluteRejectsEmptyDirectories(t *testing.T) {
	for _, cfg := range []Config{{StateDir: "", DataDir: "downloads"}, {StateDir: "state", DataDir: " "}} {
		if _, err := cfg.Absolute(); err == nil {
			t.Fatalf("accepted empty directory: %+v", cfg)
		}
	}
}

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("CEPM_HOME", t.TempDir())
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Update.Interval != time.Hour || !cfg.Update.Auto || cfg.Git.StashDirty {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CEPM_HOME", home)
	content := "[update]\ninterval = \"5m\"\nauto = false\n\n[git]\nstash_dirty = true\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Update.Interval != 5*time.Minute || cfg.Update.Auto || !cfg.Git.StashDirty {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestLoadRejectsTooSmallInterval(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CEPM_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[update]\ninterval = \"5s\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Error("expected error for interval below minimum")
	}
}

// A typo must be an error, not a silently ignored key: "atuo = false" leaving
// auto update enabled is the kind of failure nobody notices.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CEPM_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[update]\natuo = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Error("an unknown config key should be a parse error")
	}
}

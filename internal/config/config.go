// Package config loads the user-editable ~/.cepm/config.toml.
package config

import (
	"fmt"
	"os"
	"time"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/be-hase/cepm/internal/paths"
)

// Config is the effective cepm configuration (defaults + config.toml).
type Config struct {
	Update UpdateConfig
	Git    GitConfig
}

type UpdateConfig struct {
	Interval time.Duration // periodic auto-update interval
	Auto     bool          // enable the host's periodic pull loop
}

type GitConfig struct {
	StashDirty bool // stash+pop around pulls of dirty working trees
}

const MinInterval = time.Minute

func defaults() *Config {
	return &Config{
		Update: UpdateConfig{Interval: 30 * time.Minute, Auto: true},
		Git:    GitConfig{StashDirty: false},
	}
}

// fileConfig mirrors the TOML shape ("30m" strings for durations, pointers to
// distinguish "absent" from zero values).
type fileConfig struct {
	Update struct {
		Interval *string `toml:"interval"`
		Auto     *bool   `toml:"auto"`
	} `toml:"update"`
	Git struct {
		StashDirty *bool `toml:"stash_dirty"`
	} `toml:"git"`
}

// Load reads ~/.cepm/config.toml, returning defaults when it does not exist.
func Load() (*Config, error) {
	path, err := paths.ConfigFile()
	if err != nil {
		return nil, err
	}
	cfg := defaults()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	var fc fileConfig
	if err := toml.Unmarshal(data, &fc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if fc.Update.Interval != nil {
		d, err := time.ParseDuration(*fc.Update.Interval)
		if err != nil {
			return nil, fmt.Errorf("%s: update.interval: %w", path, err)
		}
		if d < MinInterval {
			return nil, fmt.Errorf("%s: update.interval must be at least %s", path, MinInterval)
		}
		cfg.Update.Interval = d
	}
	if fc.Update.Auto != nil {
		cfg.Update.Auto = *fc.Update.Auto
	}
	if fc.Git.StashDirty != nil {
		cfg.Git.StashDirty = *fc.Git.StashDirty
	}
	return cfg, nil
}

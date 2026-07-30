// Package state persists cepm's machine-managed state (~/.cepm/state.json).
//
// Writers must hold the update lock (see internal/updater) so that the CLI
// and the native host never race; Save itself is atomic (temp file + rename).
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/be-hase/cepm/internal/paths"
)

const (
	TrackBranch = "branch"
	TrackTag    = "tag"
)

// Extension is a registered extension inside a repo.
type Extension struct {
	Dir  string `json:"dir"` // repo-relative, "." for repo root
	Name string `json:"name"`
	ID   string `json:"id"` // Chrome unpacked-extension ID (derived from abs path)
}

// Repo is a managed repository.
type Repo struct {
	URL        string      `json:"url"`
	Track      string      `json:"track"` // "branch" | "tag"
	Branch     string      `json:"branch,omitempty"`
	TagPattern string      `json:"tagPattern,omitempty"`
	Tag        string      `json:"tag,omitempty"` // currently checked-out tag (tag mode)
	Head       string      `json:"head"`
	LastPull   time.Time   `json:"lastPull"`
	LastError  string      `json:"lastError,omitempty"`
	Extensions []Extension `json:"extensions"`
}

// State is the root of state.json.
type State struct {
	Version int              `json:"version"`
	Repos   map[string]*Repo `json:"repos"`
}

func New() *State {
	return &State{Version: 1, Repos: map[string]*Repo{}}
}

// RepoNames returns registered repo names, sorted.
func (s *State) RepoNames() []string {
	names := make([]string, 0, len(s.Repos))
	for n := range s.Repos {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Load reads state.json, returning an empty state when the file is missing.
func Load() (*State, error) {
	path, err := paths.StateFile()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return New(), nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if s.Repos == nil {
		s.Repos = map[string]*Repo{}
	}
	return &s, nil
}

// Save writes state.json atomically.
func (s *State) Save() error {
	path, err := paths.StateFile()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

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
	"regexp"
	"sort"
	"time"

	"github.com/be-hase/cepm/internal/paths"
)

const (
	TrackBranch = "branch"
	TrackTag    = "tag"
)

// Extension is a registered extension inside a repo.
//
// The user-intent flag is stored inverted (Disabled) so that the zero value
// means "enabled": state files written before the field existed, and the
// common case, both stay implicit.
type Extension struct {
	Dir  string `json:"dir"` // repo-relative, "." for repo root
	Name string `json:"name"`
	// ID is the extension ID Chrome assigns: derived from the manifest "key"
	// when it has one, otherwise from the absolute path.
	ID string `json:"id"`
	// Key is the manifest "key", kept so that an extension with a pinned ID
	// can be recognised across directory renames.
	Key      string `json:"key,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

// Enabled reports whether the user wants this extension active ("available"
// extensions are registered but excluded from reloads and diagnostics).
func (e Extension) Enabled() bool { return !e.Disabled }

// StaleExtension records a Chrome-side entry that no longer corresponds to a
// registered directory (the dir was renamed or deleted in the repo). Chrome
// still shows a broken extension for it until the user removes it; "cepm
// cleanup" automates that and clears the record.
type StaleExtension struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`           // "renamed" | "removed"
	NewDir string `json:"newDir,omitempty"` // for renames: where it moved
}

// Repo is a managed repository.
type Repo struct {
	URL        string `json:"url"`
	Track      string `json:"track"` // "branch" | "tag"
	Branch     string `json:"branch,omitempty"`
	TagPattern string `json:"tagPattern,omitempty"`
	Tag        string `json:"tag,omitempty"` // currently checked-out tag (tag mode)
	// Prerelease opts into semver prerelease tags (v2.0.0-rc1); by default
	// only stable releases are followed.
	Prerelease bool             `json:"prerelease,omitempty"`
	Head       string           `json:"head"`
	LastPull   time.Time        `json:"lastPull"`
	LastError  string           `json:"lastError,omitempty"`
	Extensions []Extension      `json:"extensions"`
	Stale      []StaleExtension `json:"stale,omitempty"`
}

// FindExtension returns the extension at dir, or nil.
func (r *Repo) FindExtension(dir string) *Extension {
	for i := range r.Extensions {
		if r.Extensions[i].Dir == dir {
			return &r.Extensions[i]
		}
	}
	return nil
}

// AddStale records a stale Chrome entry, deduplicating by ID.
func (r *Repo) AddStale(s StaleExtension) {
	for _, existing := range r.Stale {
		if existing.ID == s.ID {
			return
		}
	}
	r.Stale = append(r.Stale, s)
}

// RemoveStale drops the stale record with the given ID.
func (r *Repo) RemoveStale(id string) {
	out := r.Stale[:0]
	for _, s := range r.Stale {
		if s.ID != id {
			out = append(out, s)
		}
	}
	r.Stale = out
	if len(r.Stale) == 0 {
		r.Stale = nil
	}
}

// State is the root of state.json.
type State struct {
	Version int              `json:"version"`
	Repos   map[string]*Repo `json:"repos"`
	// Orphans are stale Chrome entries whose repository is gone (uninstalled
	// before the user removed them from Chrome). Keeping them here is what
	// lets "cepm cleanup" still finish the job later.
	Orphans []StaleExtension `json:"orphans,omitempty"`
}

// AddOrphans records stale entries that outlived their repository.
func (s *State) AddOrphans(list []StaleExtension) {
	for _, o := range list {
		found := false
		for _, existing := range s.Orphans {
			if existing.ID == o.ID {
				found = true
				break
			}
		}
		if !found {
			s.Orphans = append(s.Orphans, o)
		}
	}
}

// RemoveOrphan drops the orphan record with the given ID.
func (s *State) RemoveOrphan(id string) {
	out := s.Orphans[:0]
	for _, o := range s.Orphans {
		if o.ID != id {
			out = append(out, o)
		}
	}
	s.Orphans = out
	if len(s.Orphans) == 0 {
		s.Orphans = nil
	}
}

func New() *State {
	return &State{Version: Version, Repos: map[string]*Repo{}}
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
	if s.Version > Version {
		return nil, fmt.Errorf("%s was written by a newer cepm (state version %d); upgrade cepm", path, s.Version)
	}
	if s.Repos == nil {
		s.Repos = map[string]*Repo{}
	}
	// Validate rather than trust: a null entry would panic every caller that
	// dereferences it — including the native host, whose crash loop leaves no
	// diagnosis behind — and a name with path separators would let uninstall
	// delete an arbitrary directory.
	for name, r := range s.Repos {
		if r == nil {
			return nil, fmt.Errorf("%s: repository %q has no data (edit or delete the file)", path, name)
		}
		if !ValidRepoName(name) {
			return nil, fmt.Errorf("%s: invalid repository name %q", path, name)
		}
	}
	return &s, nil
}

// Version is the state schema version this build writes and understands.
const Version = 1

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidRepoName reports whether name is safe to use as a directory under
// ~/.cepm/repos (no separators, no traversal).
func ValidRepoName(name string) bool {
	return name != "." && name != ".." && repoNameRe.MatchString(name)
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

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
	"github.com/be-hase/cepm/internal/term"
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
	// KeptClones are repository *names* whose clone was left on disk because
	// Chrome might still be loading it. A repair takes several uninstalls,
	// and without this the earlier ones would be forgotten and their files
	// left behind forever. Names, not paths: this file is user-editable, and
	// whatever ends up here is eventually shown next to "rm -rf" — a name
	// passes the same validation as a registered repository and can only
	// ever resolve to one directory under ~/.cepm/repos.
	KeptClones []string `json:"keptClones,omitempty"`
}

// KeepClone records a repository name whose clone stays on disk, ignoring
// duplicates.
func (s *State) KeepClone(name string) {
	for _, n := range s.KeptClones {
		if n == name {
			return
		}
	}
	s.KeptClones = append(s.KeptClones, name)
}

// TakeKeptClones returns the kept names that are still unregistered, and
// forgets them all. A name that was registered again refers to a live clone
// now, so advising its deletion would point at the wrong thing.
func (s *State) TakeKeptClones() []string {
	var out []string
	for _, n := range s.KeptClones {
		if _, live := s.Repos[n]; !live {
			out = append(out, n)
		}
	}
	s.KeptClones = nil
	return out
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
	for _, n := range s.KeptClones {
		if !ValidRepoName(n) {
			return nil, fmt.Errorf("%s: invalid kept clone name %q (edit or delete the file)", path, term.Safe(n))
		}
	}
	s.sanitizeNames()
	return &s, nil
}

// sanitizeNames strips what a display name must never carry. Names are
// labels, not identities, so cleaning them once here is what keeps every
// caller — tables, menus, ceremonies — from having to remember. Directories
// cannot get this treatment: they must keep matching the filesystem, so
// Validate rejects them instead.
func (s *State) sanitizeNames() {
	for _, r := range s.Repos {
		for i := range r.Extensions {
			r.Extensions[i].Name = term.Strip(r.Extensions[i].Name)
		}
		for i := range r.Stale {
			r.Stale[i].Name = term.Strip(r.Stale[i].Name)
		}
	}
	for i := range s.Orphans {
		s.Orphans[i].Name = term.Strip(s.Orphans[i].Name)
	}
}

// Version is the state schema version this build writes. Every field added
// here raises it, because an older binary reads what it understands and drops
// the rest on its next write: version 2 added Extension.Key and
// State.Orphans (ids would revert to path-derived, and entries only cleanup
// can remove would vanish), version 3 added State.KeptClones (directories
// held back during a repair would stop being tracked and never be cleaned
// up).
const Version = 3

var repoNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ValidRepoName reports whether name is safe to use as a directory under
// ~/.cepm/repos (no separators, no traversal).
func ValidRepoName(name string) bool {
	return name != "." && name != ".." && repoNameRe.MatchString(name)
}

// IsLive reports whether id belongs to a currently registered extension.
func (s *State) IsLive(id string) bool {
	for _, r := range s.Repos {
		for _, e := range r.Extensions {
			if e.ID == id {
				return true
			}
		}
	}
	return false
}

// normalize enforces the one rule that ties the three id sets together: an id
// that is a registered extension right now must not also sit in a stale or
// orphan record. Extension ids are derived deterministically, so an id can
// legitimately come back — reinstalling the same repository, reverting a
// deletion — and a leftover record would make "cepm cleanup" uninstall a
// working extension. Save calls this so no writer can forget it.
func (s *State) normalize() {
	for _, r := range s.Repos {
		kept := r.Stale[:0]
		for _, st := range r.Stale {
			if !s.IsLive(st.ID) {
				kept = append(kept, st)
			}
		}
		r.Stale = kept
		if len(r.Stale) == 0 {
			r.Stale = nil
		}
	}
	kept := s.Orphans[:0]
	for _, o := range s.Orphans {
		if !s.IsLive(o.ID) {
			kept = append(kept, o)
		}
	}
	s.Orphans = kept
	if len(s.Orphans) == 0 {
		s.Orphans = nil
	}
	// The same rule for kept clones: a name registered again is a live clone,
	// and a record recommending its deletion must not outlive that.
	var clones []string
	for _, n := range s.KeptClones {
		if _, live := s.Repos[n]; !live {
			clones = append(clones, n)
		}
	}
	s.KeptClones = clones
}

// ExtRef names one registered extension: the repository and the directory
// inside it. Directories are what a user loads into Chrome, so this — not the
// repository alone — is the unit that must own an extension id.
type ExtRef struct {
	Repo string
	Dir  string
}

// String is a display form: state on disk may predate the rule that rejects
// control characters in directory names, and these refs reach the terminal
// through validation errors and doctor.
func (r ExtRef) String() string { return term.Safe(r.Repo + "/" + r.Dir) }

// DuplicateLiveID returns the id two registered extensions share, together
// with both owners, or ("", …) when every live id has exactly one owner.
// Chrome has one entity per id, so two registrations claiming the same id
// (the same manifest "key" in two places, whether or not they are in the same
// repository) would fight over it: there is no way to say which directory
// Chrome loaded, disabling one loses the other's extension too, and reload
// and doctor cannot tell them apart.
func (s *State) DuplicateLiveID() (id string, a, b ExtRef) {
	owner := map[string]ExtRef{}
	for _, name := range s.RepoNames() {
		for _, e := range s.Repos[name].Extensions {
			ref := ExtRef{Repo: name, Dir: e.Dir}
			if prev, taken := owner[e.ID]; taken {
				return e.ID, prev, ref
			}
			owner[e.ID] = ref
		}
	}
	return "", ExtRef{}, ExtRef{}
}

// IDClaims counts how many registered extensions claim each id.
func (s *State) IDClaims() map[string]int {
	claims := map[string]int{}
	for _, name := range s.RepoNames() {
		for _, e := range s.Repos[name].Extensions {
			claims[e.ID]++
		}
	}
	return claims
}

// defects counts everything that makes a state one cepm refuses to act on:
// each extra claimant of an id, and each registration whose directory name
// cannot be printed. Counting them — rather than, say, colliding ids — is
// what lets repair proceed a repository at a time, whether the file has one
// id shared three ways or nothing wrong but an unprintable name.
func (s *State) defects() int {
	total := 0
	for _, n := range s.IDClaims() {
		if n > 1 {
			total += n - 1
		}
	}
	for _, name := range s.RepoNames() {
		for _, e := range s.Repos[name].Extensions {
			if term.HasControl(e.Dir) {
				total++
			}
		}
	}
	return total
}

// SaveRepair writes a state that may still be invalid, provided it is
// strictly closer to valid than before: no id gains claimants, none becomes
// contested that was not already, and the total number of defects shrinks.
// A plain Save is all or nothing, which would reject every intermediate step
// of a repair that necessarily takes several.
func (s *State) SaveRepair(before *State) error {
	was, now := before.IDClaims(), s.IDClaims()
	for id, n := range now {
		if n > 1 && was[id] <= 1 {
			return fmt.Errorf("refusing to save: this would create a new duplicate extension id %s", id)
		}
		if n > was[id] {
			return fmt.Errorf("refusing to save: extension id %s would gain a claimant (%d → %d)", id, was[id], n)
		}
	}
	if s.defects() >= before.defects() {
		return fmt.Errorf("refusing to save: %d problem(s) before and %d after, no progress",
			before.defects(), s.defects())
	}
	s.normalize()
	s.Version = Version
	return s.save()
}

// Validate reports a state that cepm must not act on. It exists because
// earlier versions could write duplicate ids, and directory names that no
// current version would accept: the file on disk may already be
// inconsistent, and finding that out only at save time would be too late —
// by then a command may have removed something from Chrome.
func (s *State) Validate() error {
	if id, a, b := s.DuplicateLiveID(); id != "" {
		return fmt.Errorf("%s and %s both claim extension id %s "+
			"(they pin the same manifest \"key\"); uninstall one of them", a, b, id)
	}
	for _, name := range s.RepoNames() {
		for _, e := range s.Repos[name].Extensions {
			if term.HasControl(e.Dir) {
				// Such a directory reaches the terminal through every
				// suggestion cepm prints; newer versions refuse to register
				// one, but a file written earlier can still hold it.
				return fmt.Errorf("%s has control characters in its directory name; "+
					"uninstall %s and re-install it", ExtRef{Repo: name, Dir: e.Dir}, term.Quote(name))
			}
		}
	}
	return nil
}

// Save writes state.json atomically.
func (s *State) Save() error {
	if err := s.Validate(); err != nil {
		return fmt.Errorf("refusing to save: %w", err)
	}
	s.normalize()
	s.Version = Version
	return s.save()
}

// createTemp is a variable so tests can make a save fail (disk full, a
// permission lost mid-run). Provoking that through the filesystem is not
// possible: EnsureLayout deliberately repairs directory permissions on every
// lock acquisition.
var createTemp = os.CreateTemp

// FailSaves makes every save fail until the returned restore function is
// called. Test-only; exported because the callers that must not delete things
// on a failed save live in other packages.
func FailSaves() (restore func()) {
	orig := createTemp
	createTemp = func(dir, pattern string) (*os.File, error) {
		return nil, fmt.Errorf("injected save failure")
	}
	return func() { createTemp = orig }
}

func (s *State) save() error {
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
	tmp, err := createTemp(filepath.Dir(path), ".state-*.json")
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

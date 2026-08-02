package state

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/be-hase/cepm/internal/extid"
	"github.com/be-hase/cepm/internal/paths"
)

// Fixture ids pinned by manifest keys, because Validate re-derives every
// live id from its recorded key or path.
func fixtureKey(seed string) (key, id string) {
	return base64.StdEncoding.EncodeToString([]byte(seed)),
		extid.FromPublicKey([]byte(seed))
}

func TestLoadMissingReturnsEmpty(t *testing.T) {
	t.Setenv("CEPM_HOME", t.TempDir())
	s, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if s.Version != Version || len(s.Repos) != 0 {
		t.Errorf("unexpected empty state: %+v", s)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("CEPM_HOME", t.TempDir())
	key, id := fixtureKey("round-trip")
	s := New()
	s.Repos["mytools"] = &Repo{
		URL:        "git@example.com:team/mytools.git",
		Track:      TrackTag,
		Tag:        "v1.2.0",
		TagPattern: "v*",
		Head:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		LastPull:   time.Now().Truncate(time.Second),
		Extensions: []Extension{
			{Dir: "ext/a", Name: "Ext A", ID: id, Key: key},
		},
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	r := got.Repos["mytools"]
	if r == nil || r.Tag != "v1.2.0" || r.Track != TrackTag || len(r.Extensions) != 1 {
		t.Errorf("round trip mismatch: %+v", r)
	}
}

// state.json is machine-written but user-editable, and the native host reads
// it on every connect: a malformed file must produce an error, never a panic
// (a crashing host restarts forever and leaves no diagnosis).
func TestLoadRejectsMalformedState(t *testing.T) {
	cases := map[string]string{
		"null repo entry": `{"version":6,"repos":{"broken":null}}`,
		"traversal name":  `{"version":6,"repos":{"../../evil":{"url":"u"}}}`,
		"separator name":  `{"version":6,"repos":{"a/b":{"url":"u"}}}`,
		"invalid json":    `{"version":6,`,
		"future version":  `{"version":99,"repos":{}}`,
		"older version":   `{"version":3,"repos":{}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CEPM_HOME", home)
			if err := os.WriteFile(filepath.Join(home, "state.json"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := Load()
			if err == nil {
				t.Fatalf("Load should reject this state; got %+v", s)
			}
		})
	}
}

// The three id sets must stay disjoint: an id that is registered again (a
// reinstall, a reverted deletion) is no longer stale or orphaned, and leaving
// it recorded would let cleanup uninstall a working extension.
func TestSaveDropsRecordsForLiveIDs(t *testing.T) {
	t.Setenv("CEPM_HOME", t.TempDir())
	key, id := fixtureKey("live-again")
	s := New()
	s.Repos["tools"] = &Repo{
		URL: "u", Track: TrackBranch, Branch: "main", Head: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Extensions: []Extension{{Dir: "ext", Name: "Ext", ID: id, Key: key}},
	}
	s.Repos["tools"].AddStale(StaleExtension{ID: id, Name: "Ext", Reason: "removed"})
	s.AddOrphans([]StaleExtension{{ID: id, Name: "Ext", Reason: "uninstalled"}})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos["tools"].Stale) != 0 {
		t.Errorf("stale record for a live id survived: %+v", got.Repos["tools"].Stale)
	}
	if len(got.Orphans) != 0 {
		t.Errorf("orphan record for a live id survived: %+v", got.Orphans)
	}
}

// Chrome has one entity per extension id, so two live registrations with the
// same id (the same manifest "key" in two places) cannot coexist: whichever
// repo is uninstalled first would take the other's extension with it.
func TestSaveRefusesDuplicateLiveIDs(t *testing.T) {
	t.Setenv("CEPM_HOME", t.TempDir())

	// Across repositories.
	s := New()
	s.Repos["a"] = &Repo{URL: "u1", Track: TrackBranch, Branch: "main",
		Extensions: []Extension{{Dir: "ext", Name: "One", ID: "xxxx", Key: "K"}}}
	s.Repos["b"] = &Repo{URL: "u2", Track: TrackBranch, Branch: "main",
		Extensions: []Extension{{Dir: "other", Name: "Two", ID: "xxxx", Key: "K"}}}
	if err := s.Save(); err == nil {
		t.Error("Save must refuse two live extensions with the same id")
	}

	// And within one repository: two directories can pin the same key, and
	// the resulting ambiguity is identical — Chrome still has one entity.
	s2 := New()
	s2.Repos["a"] = &Repo{URL: "u1", Track: TrackBranch, Branch: "main",
		Extensions: []Extension{
			{Dir: "one", Name: "One", ID: "xxxx", Key: "K"},
			{Dir: "two", Name: "Two", ID: "xxxx", Key: "K"},
		}}
	if err := s2.Save(); err == nil {
		t.Error("Save must refuse duplicate ids inside a single repository")
	}
	// The report has to name the directories, not just the repository.
	id, a, b := s2.DuplicateLiveID()
	if id != "xxxx" || a.Dir == b.Dir {
		t.Errorf("DuplicateLiveID should identify both directories, got %s: %s and %s", id, a, b)
	}
}

func TestStaleBookkeeping(t *testing.T) {
	r := &Repo{}
	r.AddStale(StaleExtension{ID: "aaa", Name: "A", Reason: "removed"})
	r.AddStale(StaleExtension{ID: "aaa", Name: "A", Reason: "renamed"}) // duplicate
	r.AddStale(StaleExtension{ID: "bbb", Name: "B", Reason: "removed"})
	if len(r.Stale) != 2 {
		t.Fatalf("AddStale should dedupe by id, got %+v", r.Stale)
	}
	r.RemoveStale("aaa")
	if len(r.Stale) != 1 || r.Stale[0].ID != "bbb" {
		t.Fatalf("RemoveStale removed the wrong entry: %+v", r.Stale)
	}
	r.RemoveStale("bbb")
	if r.Stale != nil {
		t.Errorf("emptied Stale should be nil, got %+v", r.Stale)
	}
}

// The file records which repositories a background process will pull and
// which extensions it may touch, so keep it owner-only — including the
// directory when Save is the one creating it (EnsureLayout usually does,
// but a writer must not depend on having gone through it).
func TestSaveIsOwnerOnly(t *testing.T) {
	home := filepath.Join(t.TempDir(), "cepm")
	t.Setenv("CEPM_HOME", home)
	if err := New().Save(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(home, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("state.json mode = %o, want no group/other access", perm)
	}
	dir, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("state dir mode = %o, want no group/other access", perm)
	}
}

func TestRepoNamesSorted(t *testing.T) {
	s := New()
	s.Repos["zeta"] = &Repo{}
	s.Repos["alpha"] = &Repo{}
	names := s.RepoNames()
	if len(names) != 2 || names[0] != "alpha" || names[1] != "zeta" {
		t.Errorf("RepoNames = %v", names)
	}
}

// Validate has to check everything the current cepm writes, because LoadValid
// is also the native host's authorization: a hand-edited state naming some
// other installed extension's id would otherwise put that id under cepm's
// reload/uninstall control. The base fixture is valid; each case breaks one
// invariant and must be rejected by Validate and refused by Save.
func TestValidateRejectsStructurallyBrokenStates(t *testing.T) {
	t.Setenv("CEPM_HOME", t.TempDir())
	key, id := fixtureKey("valid")
	_, otherID := fixtureKey("someone-else")
	base := func() *State {
		s := New()
		s.Repos["tools"] = &Repo{URL: "u", Track: TrackBranch, Branch: "main", Head: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Extensions: []Extension{{Dir: "ext", Name: "Ext", ID: id, Key: key}}}
		return s
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("the base fixture must be valid: %v", err)
	}

	cases := map[string]func(*State){
		"empty url":            func(s *State) { s.Repos["tools"].URL = "" },
		"control char url":     func(s *State) { s.Repos["tools"].URL = "u\x1b[2K" },
		"unknown track":        func(s *State) { s.Repos["tools"].Track = "rolling" },
		"branch without name":  func(s *State) { s.Repos["tools"].Branch = "" },
		"control char branch":  func(s *State) { s.Repos["tools"].Branch = "main\nFORGED" },
		"empty dir":            func(s *State) { s.Repos["tools"].Extensions[0].Dir = "" },
		"traversal dir":        func(s *State) { s.Repos["tools"].Extensions[0].Dir = "../outside" },
		"absolute dir":         func(s *State) { s.Repos["tools"].Extensions[0].Dir = "/tmp/x" },
		"unnormalized dir":     func(s *State) { s.Repos["tools"].Extensions[0].Dir = "ext/../ext" },
		"malformed id":         func(s *State) { s.Repos["tools"].Extensions[0].ID = "not-an-id" },
		"well-formed wrong id": func(s *State) { s.Repos["tools"].Extensions[0].ID = otherID },
		"undecodable key":      func(s *State) { s.Repos["tools"].Extensions[0].Key = "%%%" },
		"duplicate dir": func(s *State) {
			k2, id2 := fixtureKey("second")
			s.Repos["tools"].Extensions = append(s.Repos["tools"].Extensions,
				Extension{Dir: "ext", Name: "Two", ID: id2, Key: k2})
		},
		"short head":       func(s *State) { s.Repos["tools"].Head = "abc123" },
		"option-like head": func(s *State) { s.Repos["tools"].Head = "--output=/tmp/pwned" },
		"empty head":       func(s *State) { s.Repos["tools"].Head = "" },
		"malformed stale id": func(s *State) {
			s.Repos["tools"].Stale = []StaleExtension{{ID: "zzzz", Name: "S", Reason: "removed"}}
		},
		"control char stale reason": func(s *State) {
			_, id2 := fixtureKey("stale-src")
			s.Repos["tools"].Stale = []StaleExtension{{ID: id2, Name: "S", Reason: "removed\x1b[2K"}}
		},
		"malformed orphan id": func(s *State) {
			s.Orphans = []StaleExtension{{ID: "zz", Name: "O", Reason: "uninstalled"}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := base()
			mutate(s)
			if err := s.Validate(); err == nil {
				t.Error("Validate should reject this state")
			}
			if err := s.Save(); err == nil {
				t.Error("Save should refuse this state")
			}
		})
	}
}

// A path-derived id (no manifest key) validates against the id recomputed
// from the clone's location — the design's foundation, so pin it here.
func TestValidateAcceptsPathDerivedIDs(t *testing.T) {
	t.Setenv("CEPM_HOME", t.TempDir())
	// Deliberately not filepath.Join(home, ...): the id has to match the
	// path Chrome loads from, which is the canonical one, and t.TempDir()
	// hands out a path with symlinks in it on macOS.
	repos, err := paths.ReposDir()
	if err != nil {
		t.Fatal(err)
	}
	id, err := extid.FromPath(filepath.Join(repos, "tools", "ext"))
	if err != nil {
		t.Fatal(err)
	}
	s := New()
	s.Repos["tools"] = &Repo{URL: "u", Track: TrackBranch, Branch: "main", Head: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Extensions: []Extension{{Dir: "ext", Name: "Ext", ID: id}}}
	if err := s.Validate(); err != nil {
		t.Errorf("a correctly path-derived id must validate: %v", err)
	}
	s.Repos["tools"].Extensions[0].ID = id[1:] + "a" // still well-formed, wrong value
	if err := s.Validate(); err == nil {
		t.Error("a well-formed but underivable id must be rejected")
	}
}

// Version 6 is the first public baseline: it is what released cepm binaries
// write, so it must keep loading forever. The fixture is representative —
// both track modes, enabled/disabled key-pinned extensions, stale and
// orphan records — and this contract pins that Load and Validate accept it
// with every field preserved. When Version 7 exists, this same file must
// keep loading through its migration (see AGENTS.md).
func TestVersion6BaselineFixtureLoads(t *testing.T) {
	t.Setenv("CEPM_HOME", t.TempDir())
	fixture, err := os.ReadFile(filepath.Join("testdata", "state-v6.json"))
	if err != nil {
		t.Fatal(err)
	}
	home := os.Getenv("CEPM_HOME")
	if err := os.WriteFile(filepath.Join(home, "state.json"), fixture, 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := LoadValid()
	if err != nil {
		t.Fatalf("the v6 baseline must load and validate: %v", err)
	}

	mustTime := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	// The whole persisted shape, compared exactly: a future migration that
	// drops or rewrites any v6 field fails here, not at a user's machine.
	want := &State{
		Version: 6,
		Repos: map[string]*Repo{
			"tools": {
				URL: "https://git.example.com/team/tools.git", Track: TrackBranch, Branch: "main",
				Head:      strings.Repeat("a", 40),
				LastPull:  mustTime("2026-08-01T12:00:00Z"),
				LastError: "fetch: connection timed out",
				Extensions: []Extension{
					{Dir: "ext/alpha", Name: "Alpha", ID: "jnndgchnmfmbidammmghalgbfcogokio", Key: "Zml4dHVyZS1hbHBoYQ=="},
					{Dir: "ext/beta", Name: "Beta", ID: "iidiaccbihdcdlmomgiagchjkapjlmal", Key: "Zml4dHVyZS1iZXRh", Disabled: true},
				},
				Stale: []StaleExtension{
					{ID: "abcdefghijklmnopabcdefghijklmnop", Name: "Old Widget", Reason: "removed"},
					{ID: "bacdefghijklmnopabcdefghijklmnop", Name: "Moved Widget", Reason: "renamed", NewDir: "ext/alpha"},
				},
			},
			"release": {
				URL: "https://git.example.com/team/release.git", Track: TrackTag,
				TagPattern: "v*", Tag: "v1.2.0", Prerelease: true,
				Head:     strings.Repeat("b", 40),
				LastPull: mustTime("2026-08-01T12:05:00Z"),
				Extensions: []Extension{
					{Dir: ".", Name: "Release Tool", ID: "bbijpodelfmiddmhfjokfbhghibgbhno", Key: "Zml4dHVyZS1nYW1tYQ=="},
				},
			},
		},
		Orphans: []StaleExtension{
			{ID: "ponmlkjihgfedcbaponmlkjihgfedcba", Name: "Uninstalled Tool", Reason: "uninstalled"},
		},
	}
	if !reflect.DeepEqual(st, want) {
		t.Errorf("the loaded v6 baseline differs from the fixture contract:\ngot:  %#v\nwant: %#v", st, want)
		for name := range want.Repos {
			if !reflect.DeepEqual(st.Repos[name], want.Repos[name]) {
				t.Errorf("repo %q:\ngot:  %+v\nwant: %+v", name, st.Repos[name], want.Repos[name])
			}
		}
	}
}

// A file written by a newer cepm is refused with upgrade advice, never
// half-read: the newer schema may mean something this build cannot.
func TestLoadRefusesANewerStateVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CEPM_HOME", home)
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte(`{"version":7,"repos":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "newer") || !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("a newer state version must be refused with upgrade advice, got: %v", err)
	}
}

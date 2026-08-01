package state

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/be-hase/cepm/internal/extid"
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
		"null repo entry": `{"version":5,"repos":{"broken":null}}`,
		"traversal name":  `{"version":5,"repos":{"../../evil":{"url":"u"}}}`,
		"separator name":  `{"version":5,"repos":{"a/b":{"url":"u"}}}`,
		"invalid json":    `{"version":5,`,
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
	s.Repos["tools"].AddStale(StaleExtension{ID: id, Name: "Ext", Reason: "removed", SrcDir: "ext", SrcKey: key})
	s.AddOrphans([]StaleExtension{{ID: id, Name: "Ext", Reason: "uninstalled", SrcRepo: "tools", SrcDir: "ext", SrcKey: key}})
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
// which extensions it may touch, so keep it owner-only.
func TestSaveIsOwnerOnly(t *testing.T) {
	home := t.TempDir()
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
			s.Repos["tools"].Stale = []StaleExtension{{ID: "zzzz", Name: "S", Reason: "removed", SrcDir: "ext"}}
		},
		"underivable stale id": func(s *State) {
			_, sid := fixtureKey("someone-else")
			s.Repos["tools"].Stale = []StaleExtension{{ID: sid, Name: "S", Reason: "removed", SrcDir: "gone", SrcKey: key}}
		},
		"orphan without source repo": func(s *State) {
			_, oid := fixtureKey("someone-else")
			s.Orphans = []StaleExtension{{ID: oid, Name: "O", Reason: "uninstalled", SrcDir: "ext"}}
		},
		"underivable orphan id": func(s *State) {
			_, oid := fixtureKey("someone-else")
			s.Orphans = []StaleExtension{{ID: oid, Name: "O", Reason: "uninstalled", SrcRepo: "tools", SrcDir: "ext"}}
		},
		"control char stale reason": func(s *State) {
			k2, id2 := fixtureKey("stale-src")
			s.Repos["tools"].Stale = []StaleExtension{{ID: id2, Name: "S", Reason: "removed\x1b[2K", SrcDir: "gone", SrcKey: k2}}
		},
		"malformed orphan id": func(s *State) {
			s.Orphans = []StaleExtension{{ID: "zz", Name: "O", Reason: "uninstalled", SrcRepo: "tools", SrcDir: "ext"}}
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
	home := t.TempDir()
	t.Setenv("CEPM_HOME", home)
	id, err := extid.FromPath(filepath.Join(home, "repos", "tools", "ext"))
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

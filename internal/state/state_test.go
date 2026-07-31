package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
	s := New()
	s.Repos["mytools"] = &Repo{
		URL:        "git@example.com:team/mytools.git",
		Track:      TrackTag,
		Tag:        "v1.2.0",
		TagPattern: "v*",
		Head:       "abc123",
		LastPull:   time.Now().Truncate(time.Second),
		Extensions: []Extension{
			{Dir: "ext/a", Name: "Ext A", ID: "aaaabbbbccccddddeeeeffffgggghhhh"},
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
		"null repo entry": `{"version":1,"repos":{"broken":null}}`,
		"traversal name":  `{"version":1,"repos":{"../../evil":{"url":"u"}}}`,
		"separator name":  `{"version":1,"repos":{"a/b":{"url":"u"}}}`,
		"invalid json":    `{"version":1,`,
		"future version":  `{"version":99,"repos":{}}`,
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

// A version 1 file (no Extension.Key, no Orphans) must load and be rewritten
// as version 2, so an older binary refuses it instead of silently dropping
// the new fields on its next write.
func TestLoadMigratesVersionOne(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CEPM_HOME", home)
	v1 := `{"version":1,"repos":{"tools":{"url":"u","track":"branch","branch":"main",
	  "head":"abc","extensions":[{"dir":"ext","name":"Ext","id":"aaaa"}]}}}`
	if err := os.WriteFile(filepath.Join(home, "state.json"), []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load()
	if err != nil {
		t.Fatalf("a version 1 file must still load: %v", err)
	}
	if len(s.Repos["tools"].Extensions) != 1 {
		t.Fatalf("migration lost data: %+v", s.Repos["tools"])
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if again.Version != Version {
		t.Errorf("saved version = %d, want %d", again.Version, Version)
	}
}

// The three id sets must stay disjoint: an id that is registered again (a
// reinstall, a reverted deletion) is no longer stale or orphaned, and leaving
// it recorded would let cleanup uninstall a working extension.
func TestSaveDropsRecordsForLiveIDs(t *testing.T) {
	t.Setenv("CEPM_HOME", t.TempDir())
	s := New()
	s.Repos["tools"] = &Repo{
		URL: "u", Track: TrackBranch, Branch: "main",
		Extensions: []Extension{{Dir: "ext", Name: "Ext", ID: "aaaa"}},
	}
	s.Repos["tools"].AddStale(StaleExtension{ID: "aaaa", Name: "Ext", Reason: "removed"})
	s.AddOrphans([]StaleExtension{{ID: "aaaa", Name: "Ext", Reason: "uninstalled"}})
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
	s := New()
	s.Repos["a"] = &Repo{URL: "u1", Track: TrackBranch, Branch: "main",
		Extensions: []Extension{{Dir: "ext", Name: "One", ID: "xxxx", Key: "K"}}}
	s.Repos["b"] = &Repo{URL: "u2", Track: TrackBranch, Branch: "main",
		Extensions: []Extension{{Dir: "other", Name: "Two", ID: "xxxx", Key: "K"}}}
	if err := s.Save(); err == nil {
		t.Fatal("Save must refuse two live extensions with the same id")
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

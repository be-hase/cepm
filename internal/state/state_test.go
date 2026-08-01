package state

import (
	"os"
	"path/filepath"
	"strconv"
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
		"null repo entry": `{"version":4,"repos":{"broken":null}}`,
		"traversal name":  `{"version":4,"repos":{"../../evil":{"url":"u"}}}`,
		"separator name":  `{"version":4,"repos":{"a/b":{"url":"u"}}}`,
		"invalid json":    `{"version":4,`,
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

// Losing a kept clone record would leave repair leftovers untracked, so the
// entries must survive a save/load cycle intact.
func TestKeptClonesSurviveARoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CEPM_HOME", home)
	s := New()
	s.KeepClone("old-tools", "aabb")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	again, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(again.KeptClones) != 1 || again.KeptClones[0].Name != "old-tools" || again.KeptClones[0].Token != "aabb" {
		t.Errorf("KeptClones did not survive a round trip: %+v", again.KeptClones)
	}
	if got := again.TakeKeptClones(); len(got) != 1 || again.KeptClones != nil {
		t.Errorf("TakeKeptClones should hand them over once: %+v / %+v", got, again.KeptClones)
	}
}

// A kept name resolves to a directory that is eventually shown next to
// "rm -rf", and state.json is user-editable: what does not pass the same
// validation as a repository name must never be resolved, and a name that
// was registered again points at a live clone now.
func TestKeptClonesAreValidatedAndDroppedWhenLiveAgain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CEPM_HOME", home)

	for _, bad := range []string{"../evil", "a/b", "/tmp", ".."} {
		raw := `{"version":4,"repos":{},"keptClones":[{"name":` + strconv.Quote(bad) + `}]}`
		if err := os.WriteFile(filepath.Join(home, "state.json"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(); err == nil {
			t.Errorf("Load should reject kept clone name %q", bad)
		}
	}

	// Re-registered name: both Save and TakeKeptClones must drop it.
	s := New()
	s.KeepClone("tools", "tok")
	s.Repos["tools"] = &Repo{URL: "u", Track: TrackBranch, Branch: "main"}
	if got := s.TakeKeptClones(); len(got) != 0 {
		t.Errorf("a re-registered name must not be handed out for deletion: %v", got)
	}
	s.KeepClone("tools", "tok")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	saved, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.KeptClones) != 0 {
		t.Errorf("Save should drop kept names that are registered again: %v", saved.KeptClones)
	}

	// A name Load would refuse must be refused on the way in as well —
	// recording it would build a state file this very cepm then rejects.
	s2 := New()
	s2.KeepClone("../evil", "tok")
	if len(s2.KeptClones) != 0 {
		t.Errorf("KeepClone must refuse an invalid name: %+v", s2.KeptClones)
	}
	s2.KeptClones = []KeptClone{{Name: "../evil"}} // bypassing KeepClone
	if err := s2.Save(); err == nil {
		t.Error("Save must refuse a kept clone name that would not load back")
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

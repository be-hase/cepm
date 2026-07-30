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
	if s.Version != 1 || len(s.Repos) != 0 {
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

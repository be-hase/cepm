package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/paths"
	"github.com/be-hase/cepm/internal/state"
)

// symlinkedRepos points ~/.cepm/repos at a directory elsewhere — the layout
// someone sets up to keep clones off the boot volume — and returns where it
// really lives.
func symlinkedRepos(t *testing.T) (home, target string) {
	t.Helper()
	home, err := os.MkdirTemp("/tmp", "cepm-home")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	target, err = os.MkdirTemp("/tmp", "cepm-repos")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(target) })
	t.Setenv("CEPM_HOME", home)
	if err := os.Symlink(target, filepath.Join(home, "repos")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	return home, target
}

// reset moves repos/ into a backup. With repos/ a symlink, resolving it
// first would move the *target* and leave a dangling link behind — after
// which EnsureLayout's MkdirAll fails with "file exists" and cepm cannot
// start over at all, which is the one thing reset is for.
func TestResetMovesTheSymlinkNotWhatItPointsAt(t *testing.T) {
	interactive(t)
	home, target := symlinkedRepos(t)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	if err := os.MkdirAll(filepath.Join(target, "tools", "ext"), 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "yes\n", "reset")
	if err != nil {
		t.Fatalf("reset: %v\n%s", err, out)
	}

	// The link is gone from the home, not left dangling behind a moved
	// target — the next install has to be able to create repos/ again.
	if _, err := os.Lstat(filepath.Join(home, "repos")); !os.IsNotExist(err) {
		t.Errorf("repos/ should have been moved away whole, Lstat err = %v", err)
	}
	// A dangling link is what would break this: MkdirAll fails with "file
	// exists" on one, so cepm could never lay out its home again.
	if err := paths.EnsureLayout(); err != nil {
		t.Fatalf("cepm cannot lay out its home again after a reset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "repos")); err != nil {
		t.Errorf("repos/ was not recreated: %v", err)
	}
}

// The final step of an install is one rename from staging to repos/<name>,
// and staging next to repos/ makes that rename cross devices whenever repos/
// points at another volume — EXDEV, so install fails outright for that whole
// layout. A second filesystem cannot be conjured in a test, so the decision
// is forced here instead; what it proves is that the fallback path works and
// leaves nothing behind.
// Where the staging goes is the whole decision, so assert it directly: an
// end-to-end install passes either way on a single filesystem, which is all
// a test machine has.
func TestStagingFollowsReposOntoItsOwnFilesystem(t *testing.T) {
	prev := sameFilesystem
	t.Cleanup(func() { sameFilesystem = prev })

	sameFilesystem = func(string, string) bool { return true }
	if got := stagingParentFor("/home/.cepm", "/home/.cepm/repos"); got != "/home/.cepm" {
		t.Errorf("same filesystem: staged in %q, want beside repos/ so a reset cannot take it", got)
	}
	sameFilesystem = func(string, string) bool { return false }
	if got := stagingParentFor("/home/.cepm", "/volume/repos"); got != "/volume/repos" {
		t.Errorf("another filesystem: staged in %q, want inside repos/ — the final rename cannot cross devices", got)
	}
}

func TestInstallWorksWhenReposIsOnAnotherFilesystem(t *testing.T) {
	startFakeHost(t)
	_, target := symlinkedRepos(t)
	prev := sameFilesystem
	t.Cleanup(func() { sameFilesystem = prev })
	sameFilesystem = func(string, string) bool { return false }
	origin := localOrigin(t, `{"manifest_version":3,"name":"Ext","version":"1.0"}`)

	out, err := run(t, "", "install", origin, "--name", "tools", "--all")
	if err != nil {
		t.Fatalf("install into a symlinked repos/: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(target, "tools", "ext", "manifest.json")); err != nil {
		t.Errorf("the clone should be where repos/ really points: %v", err)
	}
	// And nothing staged is left behind inside it.
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".install-") {
			t.Errorf("staging directory left behind: %s", e.Name())
		}
	}
}

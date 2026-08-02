package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/assist"
	"github.com/be-hase/cepm/internal/paths"
	"github.com/be-hase/cepm/internal/state"
)

// localOriginWithTwoExtensions builds a repository that makes install stop
// at the selection prompt — the window a concurrent reset slips into.
func localOriginWithTwoExtensions(t *testing.T) (originPath string) {
	t.Helper()
	src := t.TempDir()
	origin := filepath.Join(src, "origin.git")
	work := filepath.Join(src, "work")
	gitCmd(t, src, "init", "-q", "--bare", "--initial-branch=main", origin)
	gitCmd(t, src, "clone", "-q", origin, work)
	gitCmd(t, work, "checkout", "-q", "-b", "main")
	for _, name := range []string{"one", "two"} {
		if err := os.MkdirAll(filepath.Join(work, name), 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := `{"manifest_version":3,"name":"` + name + `","version":"1.0"}`
		if err := os.WriteFile(filepath.Join(work, name, "manifest.json"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, work, "add", "-A")
	gitCmd(t, work, "commit", "-qm", "init")
	gitCmd(t, work, "push", "-q", "origin", "main")
	return origin
}

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

// The staging path is logical, and repos/ can stop meaning what it meant:
// a reset during the selection prompt replaces the symlink. The cleanup
// would then delete a directory on the new side and leave the real staging
// — a full clone — behind on a volume nobody looks at. Failing the install
// is the accepted trade; leaking the clone is not.
func TestAFailedInstallCleansUpTheStagingItReallyMade(t *testing.T) {
	interactive(t)
	startFakeHost(t)
	home, target := symlinkedRepos(t)
	prev := sameFilesystem
	t.Cleanup(func() { sameFilesystem = prev })
	sameFilesystem = func(string, string) bool { return false } // stage inside repos/

	origin := localOriginWithTwoExtensions(t)

	// At the selection prompt, re-point repos/ somewhere else, as a reset
	// moving it away would.
	elsewhere, err := os.MkdirTemp("/tmp", "cepm-elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(elsewhere) })
	swapped := false
	origIsTTY := assist.IsTTY
	t.Cleanup(func() { assist.IsTTY = origIsTTY })
	assist.IsTTY = func() bool {
		if !swapped {
			swapped = true
			link := filepath.Join(home, "repos")
			if err := os.Remove(link); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(elsewhere, link); err != nil {
				t.Error(err)
			}
		}
		return true
	}

	// Whether this install succeeds or fails is not the claim; what it
	// leaves behind is.
	_, _ = run(t, "1\n", "install", origin, "--name", "tools")
	if !swapped {
		t.Fatal("the test never reached the selection prompt")
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".install-") {
			t.Errorf("the real staging was left behind: %s", filepath.Join(target, e.Name()))
		}
	}
}

// sameFilesystem has to actually compare devices, not just answer "yes".
// The staging decision is proved with the function replaced, so an
// implementation that lost its comparison would still pass that — and put
// the staging where the final rename cannot reach, for every user with
// repos/ on another volume.
func TestSameFilesystemSeesTwoDevicesApart(t *testing.T) {
	// Somewhere on a different filesystem from /tmp. Not every machine has
	// one, so this contributes coverage where it can rather than requiring
	// a particular layout.
	var other string
	for _, candidate := range []string{"/dev/shm", "/run/user", "/System/Volumes/Data", "/private/var/vm"} {
		if _, err := os.Stat(candidate); err == nil && !sameFilesystem("/tmp", candidate) {
			other = candidate
			break
		}
	}
	if other == "" {
		t.Skip("no second filesystem to compare against on this machine")
	}
	if sameFilesystem("/tmp", other) {
		t.Errorf("%s and /tmp are on different devices but compared equal", other)
	}
	if !sameFilesystem("/tmp", "/tmp") {
		t.Error("a directory must be on the same filesystem as itself")
	}
}

// A relative symlink means something different once it has been moved:
// "repos -> ../repo-store" lands in backup-xxx/ still saying "../", which
// now points inside ~/.cepm at nothing. The recovery reset prints for a
// broken state — reading each clone's origin URL out of the backup — is
// exactly what stops working.
func TestResetKeepsARelativeSymlinkPointingWhereItDid(t *testing.T) {
	interactive(t)
	home, err := os.MkdirTemp("/tmp", "cepm-home")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("CEPM_HOME", home)
	// A sibling of the home, reached relatively — the shape someone gets
	// from "ln -s ../repo-store repos".
	target := filepath.Join(filepath.Dir(home), filepath.Base(home)+"-store")
	if err := os.MkdirAll(filepath.Join(target, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(target) })
	if err := os.Symlink(filepath.Join("..", filepath.Base(target)), filepath.Join(home, "repos")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})

	out, err := run(t, "yes\n", "reset")
	if err != nil {
		t.Fatalf("reset: %v\n%s", err, out)
	}

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	var backup string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "backup-") {
			backup = filepath.Join(home, e.Name())
		}
	}
	if backup == "" {
		t.Fatal("fixture: reset should have made a backup")
	}
	// The clones have to still be reachable through the moved link, which
	// is the whole promise of the backup.
	if _, err := os.Stat(filepath.Join(backup, "repos", "tools")); err != nil {
		t.Errorf("the backup no longer reaches the clones: %v", err)
	}
}

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

// reset is all or nothing: metadata in the backup with the clones still
// active is the split state it exists to resolve. The symlink handling sits
// in the middle of that loop, after state.json has already moved, so its
// failures have to return into the same rollback rather than out of the
// function.
func TestAFailedSymlinkMoveRollsBackLikeAnyOther(t *testing.T) {
	interactive(t)
	home, err := os.MkdirTemp("/tmp", "cepm-home")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("CEPM_HOME", home)
	target := filepath.Join(filepath.Dir(home), filepath.Base(home)+"-store")
	if err := os.MkdirAll(filepath.Join(target, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(target) })
	if err := os.Symlink(filepath.Join("..", filepath.Base(target)), filepath.Join(home, "repos")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	stateFile := filepath.Join(home, "state.json")
	if _, err := os.Stat(stateFile); err != nil {
		t.Fatal(err)
	}

	// Make the symlink's move fail: something already occupies its place in
	// the backup. state.json moves first, so this lands mid-loop.
	prev := renameForBackup
	t.Cleanup(func() { renameForBackup = prev })
	renameForBackup = func(src, dst string) error {
		if err := prev(src, dst); err != nil {
			return err
		}
		// The next entry is repos; block the name it is about to take.
		return os.Symlink("/nowhere", filepath.Join(filepath.Dir(dst), "repos"))
	}

	out, err := run(t, "yes\n", "reset")
	if err == nil {
		t.Fatalf("reset should have failed on the blocked symlink:\n%s", out)
	}
	if _, serr := os.Stat(stateFile); serr != nil {
		t.Errorf("state.json must have been put back, got %v", serr)
	}
	if _, lerr := os.Lstat(filepath.Join(home, "repos")); lerr != nil {
		t.Errorf("repos must still be in place, got %v", lerr)
	}
}

// The end-to-end counterpart: the fallback staging really does produce a
// working install and leaves nothing behind. A second filesystem cannot be
// conjured in a test, so the decision is forced rather than arranged.
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

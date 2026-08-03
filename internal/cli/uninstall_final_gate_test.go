package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"

	"github.com/be-hase/cepm/internal/assist"
	"github.com/be-hase/cepm/internal/paths"
	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/testgit"
)

// findTrashedClone locates the clone inside the trash a test's uninstall
// left, failing if there is none.
func findTrashedClone(t *testing.T, name string) string {
	t.Helper()
	home, err := paths.CepmDir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".trash-") {
			return filepath.Join(home, e.Name(), name)
		}
	}
	t.Fatal("no trash directory was created")
	return ""
}

// The clone is moved, never deleted. This is what ended the arms race of
// detecting every way a clone can change while a dialog is open: commits,
// stashes, rewrites of already-dirty files — whatever happened, it is all
// still there in the trash.
func TestUninstallMovesTheCloneToTheTrashWithEverythingInIt(t *testing.T) {
	interactive(t)
	startFakeHost(t, idA)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}

	// Work arrives in every form git can hold it, *while* the Chrome
	// question is open — after any pre-flight look at the tree.
	arrived := false
	origIsTTY := assist.IsTTY
	t.Cleanup(func() { assist.IsTTY = origIsTTY })
	assist.IsTTY = func() bool {
		if !arrived {
			arrived = true
			if werr := os.WriteFile(filepath.Join(dir, "committed.txt"), []byte("commit me\n"), 0o644); werr != nil {
				t.Error(werr)
			}
			gitCmd(t, dir, "add", "-A")
			gitCmd(t, dir, "commit", "-qm", "mid-dialog commit")
			if werr := os.WriteFile(filepath.Join(dir, "stashed.txt"), []byte("stash me\n"), 0o644); werr != nil {
				t.Error(werr)
			}
			gitCmd(t, dir, "stash", "push", "-u", "-m", "mid-dialog stash")
			if werr := os.WriteFile(filepath.Join(dir, "loose.txt"), []byte("just here\n"), 0o644); werr != nil {
				t.Error(werr)
			}
		}
		return true
	}

	out, err := run(t, "n\n", "uninstall", "tools")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if !arrived {
		t.Fatal("the test never reached a dialog")
	}
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Errorf("the clone should be gone from repos/, stat err = %v", serr)
	}

	moved := findTrashedClone(t, "tools")
	if !strings.Contains(out, ".trash-") {
		t.Errorf("the trash location must be reported:\n%s", out)
	}
	for _, f := range []string{"committed.txt", "loose.txt"} {
		if _, serr := os.Stat(filepath.Join(moved, f)); serr != nil {
			t.Errorf("%s must survive in the trash: %v", f, serr)
		}
	}
	// The stash lives in .git, which moved wholesale.
	stashes := strings.TrimSpace(gitOut(t, moved, "stash", "list"))
	if !strings.Contains(stashes, "mid-dialog stash") {
		t.Errorf("the mid-dialog stash must survive in the trash, got %q", stashes)
	}
}

// The move happens while the update lock is held: between a save and a move
// outside it, a reset plus an install could put a brand-new clone at the
// same logical path, and the move would take a clone this command never saw.
func TestUninstallMovesTheCloneWhileHoldingTheLock(t *testing.T) {
	interactive(t)
	startFakeHost(t)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})

	lockPath, err := paths.UpdateLockPath()
	if err != nil {
		t.Fatal(err)
	}
	moved := false
	prev := trashClone
	t.Cleanup(func() { trashClone = prev })
	trashClone = func(dir, name string) (string, error) {
		moved = true
		// flock(2) excludes other handles even in the same process, so a
		// failed TryLock here proves the update lock is held around us.
		ok, lerr := flock.New(lockPath).TryLock()
		if lerr == nil && ok {
			t.Error("the clone was moved with the update lock free")
		}
		return prev(dir, name)
	}

	out, err := run(t, "n\n", "uninstall", "tools")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if !moved {
		t.Fatal("fixture: the clone should have been moved")
	}
}

// A Ctrl-C stops the uninstall before anything is written: the state stays
// registered and the clone stays in place. The cancel lands inside the
// final lock — cancelling any earlier stops at the lock acquisition, which
// would pass with or without the check under test.
func TestUninstallStopsWhenCancelledBeforeTheMove(t *testing.T) {
	interactive(t)
	host := startFakeHost(t, idA)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	lists := 0
	cancelled := false
	host.mu.Lock()
	host.onList = func() {
		lists++
		if lists == 2 { // 1st: preparing the question; 2nd: inside the lock
			cancelled = true
			cancel()
		}
	}
	host.mu.Unlock()

	resetPromptReader()
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader("n\n"))
	root.SetArgs([]string{"uninstall", "tools"})
	err = root.ExecuteContext(ctx)
	if err == nil {
		t.Fatalf("a cancelled uninstall must not report success:\n%s", out.String())
	}
	if !cancelled {
		t.Fatal("the test never reached the second list")
	}
	if _, serr := os.Stat(dir); serr != nil {
		t.Errorf("the clone must stay in place after a cancellation: %v", serr)
	}
	st, lerr := state.Load()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if st.Repos["tools"] == nil {
		t.Error("nothing may be unregistered after a cancellation")
	}
}

// A clone that is not even a git repository moves to the trash like any
// other — there is nothing to check and nothing to lose, which is the whole
// point of moving instead of deleting.
func TestUninstallTrashesAnUncheckableCloneToo(t *testing.T) {
	startFakeHost(t)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "orphan-work.txt"), []byte("still mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "", "uninstall", "tools")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	// "Cannot tell whether it is dirty" points the caution the same way.
	if !strings.Contains(out, "may contain") {
		t.Errorf("an uncheckable clone should be flagged as possibly holding changes:\n%s", out)
	}
	moved := findTrashedClone(t, "tools")
	if _, serr := os.Stat(filepath.Join(moved, "orphan-work.txt")); serr != nil {
		t.Errorf("the work must survive in the trash: %v", serr)
	}
}

// An answer is about a registration, not a name. While a dialog is open, a
// reset plus an install can put a different repository — another URL — under
// the same name with the same path-derived ids, and the snapshot re-check
// has to notice the swap: acting would carry an answer about one
// registration onto another. The clone would survive in the trash, but
// "nothing was lost" is not "the right thing was done".
func TestUninstallRefusesARegistrationSwappedDuringTheDialogs(t *testing.T) {
	interactive(t)
	host := startFakeHost(t)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	if err := os.MkdirAll(filepath.Join(mustRepoDir(t, "tools"), "ext"), 0o755); err != nil {
		t.Fatal(err)
	}
	host.mu.Lock()
	host.loaded[idA] = true // so the Chrome question is asked
	host.mu.Unlock()

	// While the question is open: the registration is replaced wholesale —
	// same name, same extension ids (seeded directly, as a reset+install
	// would produce them path-derived), different URL.
	swapped := false
	origIsTTY := assist.IsTTY
	t.Cleanup(func() { assist.IsTTY = origIsTTY })
	assist.IsTTY = func() bool {
		if !swapped {
			swapped = true
			st, err := state.Load()
			if err != nil {
				t.Error(err)
				return true
			}
			st.Repos["tools"].URL = "https://elsewhere.example.com/other/tools.git"
			if err := st.Save(); err != nil {
				t.Error(err)
			}
		}
		return true
	}

	out, err := run(t, "n\n", "uninstall", "tools")
	if err == nil {
		t.Fatalf("an answer about one registration must not act on another:\n%s", out)
	}
	if !strings.Contains(err.Error(), "changed while") {
		t.Errorf("the error should say what happened: %v", err)
	}
	st, lerr := state.Load()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if st.Repos["tools"] == nil {
		t.Error("the swapped-in registration must survive untouched")
	}
}

// A reset plus a re-install of the *same* URL reproduces every attribute
// the state records — same name, same tracking, same path-derived ids. What
// it cannot reproduce is the directory instance: the new clone is a new
// inode, and that is what tells the answer's registration from the one that
// replaced it.
func TestUninstallRefusesAReinstalledCloneDuringTheDialogs(t *testing.T) {
	interactive(t)
	host := startFakeHost(t)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	dir := mustRepoDir(t, "tools")
	host.mu.Lock()
	host.loaded[idA] = true // so the Chrome question is asked
	host.mu.Unlock()

	// While the question is open, the clone is rebuilt in place — the
	// filesystem shape of "reset, then reinstall the same URL". The state's
	// attributes stay identical on purpose: only the instance differs.
	rebuilt := false
	origIsTTY := assist.IsTTY
	t.Cleanup(func() { assist.IsTTY = origIsTTY })
	assist.IsTTY = func() bool {
		if !rebuilt {
			rebuilt = true
			if err := os.RemoveAll(dir); err != nil {
				t.Error(err)
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Error(err)
			}
			gitCmd(t, dir, "init", "-q", "-b", "main")
		}
		return true
	}

	out, err := run(t, "n\n", "uninstall", "tools")
	if err == nil {
		t.Fatalf("an answer about one clone instance must not act on its replacement:\n%s", out)
	}
	if !strings.Contains(err.Error(), "changed while") {
		t.Errorf("the error should say what happened: %v", err)
	}
	st, lerr := state.Load()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if st.Repos["tools"] == nil {
		t.Error("the replacement registration must survive untouched")
	}
	if _, serr := os.Stat(dir); serr != nil {
		t.Errorf("the replacement clone must stay in place: %v", serr)
	}
}

func mustRepoDir(t *testing.T, name string) string {
	t.Helper()
	dir, err := updaterRepoDir(name)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// With --keep-files nothing moves after the save, so a save that is live
// but not yet flushed is the same situation install and enable carry on
// through: reporting it as failure leaves the user re-running a command
// that then says "not registered".
func TestKeepFilesUninstallSurvivesADurabilityOnlyFailure(t *testing.T) {
	interactive(t)
	startFakeHost(t)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(state.FailDurability())

	out, err := run(t, "", "uninstall", "tools", "--keep-files")
	if err != nil {
		t.Fatalf("an unflushed but written state must not fail --keep-files: %v\n%s", err, out)
	}
	if !strings.Contains(out, "not flushed") {
		t.Errorf("the crash window has to be reported:\n%s", out)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("--keep-files must keep the clone: %v", err)
	}
}

// gitOut runs git and returns its output, for assertions on repositories a
// test moved around.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = testgit.Env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

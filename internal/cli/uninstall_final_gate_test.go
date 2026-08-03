package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"

	"github.com/be-hase/cepm/internal/assist"
	"github.com/be-hase/cepm/internal/paths"
	"github.com/be-hase/cepm/internal/state"
)

// Deleting the clone after releasing the update lock leaves an instant in
// which a reset plus an install can put a brand-new clone at the same
// logical path — and the delete then takes a clone from a registration this
// uninstall never saw. The delete therefore happens inside the lock, and
// holding the lock is the only observable difference, so that is what the
// seam asserts.
func TestUninstallDeletesTheCloneWhileHoldingTheLock(t *testing.T) {
	interactive(t)
	startFakeHost(t)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	lockPath, err := paths.UpdateLockPath()
	if err != nil {
		t.Fatal(err)
	}
	deleted := false
	prev := removeClone
	t.Cleanup(func() { removeClone = prev })
	removeClone = func(path string) error {
		deleted = true
		// flock(2) excludes other handles even in the same process, so a
		// failed TryLock here proves the update lock is held around us.
		ok, lerr := flock.New(lockPath).TryLock()
		if lerr == nil && ok {
			t.Error("the clone was deleted with the update lock free")
		}
		return prev(path)
	}
	out, err := run(t, "n\n", "uninstall", "tools")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if !deleted {
		t.Fatal("fixture: the clone should have been deleted")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the clone should be gone, stat err = %v", err)
	}
}

// The dirty check runs before the dialogs, and the answer it collects covers
// the changes that were shown then. Work that lands while a dialog is open
// was never approved for deletion — a clean tree at the start must not stand
// in for a clean tree at the delete.
func TestUninstallRefusesChangesThatArrivedDuringTheDialogs(t *testing.T) {
	interactive(t)
	startFakeHost(t, idA) // loaded, so the Chrome-removal question is asked
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	// A real clean repository: the re-check compares real git status.
	gitCmd(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-qm", "init")

	// The edit lands while the Chrome-removal question is open.
	edited := false
	origIsTTY := assist.IsTTY
	t.Cleanup(func() { assist.IsTTY = origIsTTY })
	assist.IsTTY = func() bool {
		if !edited {
			edited = true
			if werr := os.WriteFile(filepath.Join(dir, "precious.txt"), []byte("work\n"), 0o644); werr != nil {
				t.Error(werr)
			}
		}
		return true
	}

	out, err := run(t, "n\n", "uninstall", "tools")
	if err == nil {
		t.Fatalf("changes that were never shown to the user must stop the delete:\n%s", out)
	}
	if !strings.Contains(err.Error(), "changed while") {
		t.Errorf("the error should say what happened, got: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "precious.txt")); serr != nil {
		t.Errorf("the unapproved work must survive: %v", serr)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Repos["tools"] == nil {
		t.Error("nothing may be unregistered when the delete was refused")
	}
}

// "git status" shows the same " M file" line no matter how many times the
// file has been rewritten since, so a status-level comparison would wave
// through a rewrite of an already-dirty file. The user approved deleting the
// content they were shown, not whatever the file holds by the time the
// dialogs close — only a content-level fingerprint can tell those apart.
func TestUninstallRefusesARewriteOfAnAlreadyDirtyFile(t *testing.T) {
	interactive(t)
	startFakeHost(t, idA)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	gitCmd(t, filepath.Dir(dir), "init", "-q", "-b", "main", dir)
	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-qm", "init")
	// Dirty from the start — the porcelain line is " M file.txt" and stays
	// exactly that through the rewrite below.
	if err := os.WriteFile(file, []byte("edit-one\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	edited := false
	origIsTTY := assist.IsTTY
	t.Cleanup(func() { assist.IsTTY = origIsTTY })
	assist.IsTTY = func() bool {
		if !edited {
			edited = true
			if werr := os.WriteFile(file, []byte("edit-two\n"), 0o644); werr != nil {
				t.Error(werr)
			}
		}
		return true
	}

	// "y" approves deleting edit-one; "n" declines the Chrome removal.
	out, err := run(t, "y\nn\n", "uninstall", "tools")
	if err == nil {
		t.Fatalf("content the user never saw must not be deleted on an old approval:\n%s", out)
	}
	b, rerr := os.ReadFile(file)
	if rerr != nil {
		t.Fatalf("the clone must survive: %v", rerr)
	}
	if string(b) != "edit-two\n" {
		t.Errorf("the rewritten content must survive, got %q", b)
	}
}

// A Ctrl-C during the dialogs or the final checks surfaces as an error from
// whatever git command was running — and a fingerprint error normally reads
// as "delete", because a corrupt clone is what uninstall cleans up. A
// cancelled caller is the exception: the Ctrl-C was pressed to stop the
// delete, not to authorise it.
func TestUninstallStopsWhenCancelledBeforeTheDelete(t *testing.T) {
	interactive(t)
	host := startFakeHost(t, idA)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// The Ctrl-C lands inside the final lock — after the lock was won, while
	// the host is being asked what Chrome still has. Cancelling any earlier
	// stops the command at the lock acquisition, which would pass with or
	// without the check under test.
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
		t.Fatal("the test never reached a dialog")
	}
	if _, serr := os.Stat(dir); serr != nil {
		t.Errorf("the clone must survive a cancellation: %v", serr)
	}
	st, lerr := state.Load()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if st.Repos["tools"] == nil {
		t.Error("nothing may be unregistered after a cancellation")
	}
}

// Committing during a dialog returns the worktree to clean, so a
// fingerprint of uncommitted changes alone compares "" == "" — while the
// clone now holds work nobody was shown. The approved identity includes
// HEAD precisely so that a commit is a change like any other.
func TestUninstallRefusesACommitMadeDuringTheDialogs(t *testing.T) {
	interactive(t)
	startFakeHost(t, idA)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}

	committed := false
	origIsTTY := assist.IsTTY
	t.Cleanup(func() { assist.IsTTY = origIsTTY })
	assist.IsTTY = func() bool {
		if !committed {
			committed = true
			// Clean before, clean after — the work moves into a commit
			// while the Chrome question is open.
			if werr := os.WriteFile(filepath.Join(dir, "work.txt"), []byte("precious\n"), 0o644); werr != nil {
				t.Error(werr)
			}
			gitCmd(t, dir, "add", "-A")
			gitCmd(t, dir, "commit", "-qm", "work done mid-dialog")
		}
		return true
	}

	out, err := run(t, "n\n", "uninstall", "tools")
	if err == nil {
		t.Fatalf("a commit made during the dialogs must stop the delete:\n%s", out)
	}
	if _, serr := os.Stat(filepath.Join(dir, "work.txt")); serr != nil {
		t.Errorf("the committed work must survive: %v", serr)
	}
	st, lerr := state.Load()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if st.Repos["tools"] == nil {
		t.Error("nothing may be unregistered when the delete was refused")
	}
}

// A tree whose local-change state cannot be checked may be the corrupt
// clone uninstall exists to clean up — or a healthy tree the fingerprint
// has no answer for, still holding work. "Could not check" therefore never
// quietly becomes "nothing to lose": headless it refuses, and interactively
// it is its own explicit question.
func TestUninstallFailsClosedOnAnUncheckableClone(t *testing.T) {
	startFakeHost(t)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	// Breaking the repository is what makes the state uncheckable.
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}

	if out, err := run(t, "", "uninstall", "tools"); err == nil {
		t.Fatalf("headless, an uncheckable clone must not be deleted:\n%s", out)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the clone must survive the refusal: %v", err)
	}

	// Interactively it is an explicit question, and "y" really does delete.
	interactive(t)
	out, err := run(t, "y\n", "uninstall", "tools")
	if err != nil {
		t.Fatalf("an explicitly approved delete must proceed: %v\n%s", err, out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the approved delete should have happened, stat err = %v", err)
	}
}

// With --keep-files nothing is destroyed after the save, so a save that is
// live but not yet flushed is the same situation install and enable carry on
// through: reporting it as failure leaves the user re-running a command that
// then says "not registered".
func TestKeepFilesUninstallSurvivesADurabilityOnlyFailure(t *testing.T) {
	interactive(t)
	startFakeHost(t)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
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

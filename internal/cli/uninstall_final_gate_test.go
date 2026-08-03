package cli

import (
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

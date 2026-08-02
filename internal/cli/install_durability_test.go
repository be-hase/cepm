package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/state"
)

// A save that fails *after* state.json was replaced is a durability
// question, not a "did it happen" question: the registration is live. Undoing
// the rename over it — as a plain error would make install do — deletes the
// clone the state now points at, which is the state-before-filesystem
// inversion the rollback exists to prevent.
func TestInstallKeepsTheCloneWhenOnlyTheFlushFailed(t *testing.T) {
	startFakeHost(t)
	origin := localOrigin(t, `{"manifest_version":3,"name":"Ext","version":"1.0"}`)
	t.Cleanup(state.FailDurability())

	out, err := run(t, "", "install", origin, "--name", "tools", "--all")
	if err != nil {
		t.Fatalf("an unflushed but written state must not fail the install: %v\n%s", err, out)
	}
	if !strings.Contains(out, "not flushed") {
		t.Errorf("the crash window has to be reported:\n%s", out)
	}

	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ext", "manifest.json")); err != nil {
		t.Errorf("the clone must stay where the saved state says it is: %v", err)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Repos["tools"] == nil {
		t.Error("the registration was written to disk; it must still be there")
	}
}

// Uninstall is the one caller that destroys something after saving, and for
// it "the new state is live" is not enough. If the flush never lands, a
// crash brings the registration back — with the clone already deleted, and
// no way to tell the state to forget it again.
func TestUninstallRefusesToDeleteTheCloneUntilTheSaveIsDurable(t *testing.T) {
	interactive(t)
	startFakeHost(t, idA)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(state.FailDurability())

	out, err := run(t, "n\n", "uninstall", "tools")
	if err == nil {
		t.Fatalf("uninstall must not proceed on a save it cannot make durable:\n%s", out)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the clone must survive: %v", err)
	}
}

// localOrigin builds a bare repository with one extension in ext/ and
// returns its path, so an install can run for real without a network.
func localOrigin(t *testing.T, manifest string) (originPath string) {
	t.Helper()
	src := t.TempDir()
	origin := filepath.Join(src, "origin.git")
	work := filepath.Join(src, "work")
	gitCmd(t, src, "init", "-q", "--bare", "--initial-branch=main", origin)
	gitCmd(t, src, "clone", "-q", origin, work)
	gitCmd(t, work, "checkout", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(work, "ext"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "ext", "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, work, "add", "-A")
	gitCmd(t, work, "commit", "-qm", "init")
	gitCmd(t, work, "push", "-q", "origin", "main")
	return origin
}

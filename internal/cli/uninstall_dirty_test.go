package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/state"
)

// A dirty clone uninstalls without a gate — the clone is moved to the
// trash, not deleted, so nothing can be lost — but the report has to say
// that local changes rode along: a user who forgot about them would
// otherwise delete the trash unread.
func TestUninstallReportsTheLocalChangesItMovedToTheTrash(t *testing.T) {
	startFakeHost(t)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "edited.js"), []byte("local work"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "", "uninstall", "tools")
	if err != nil {
		t.Fatalf("a dirty clone must uninstall (nothing is lost): %v\n%s", err, out)
	}
	if !strings.Contains(out, ".trash-") {
		t.Errorf("the trash location must be reported:\n%s", out)
	}
	if !strings.Contains(out, "uncommitted local changes") {
		t.Errorf("the report must warn that local changes moved with the clone:\n%s", out)
	}
	moved := findTrashedClone(t, "tools")
	if b, rerr := os.ReadFile(filepath.Join(moved, "edited.js")); rerr != nil || string(b) != "local work" {
		t.Errorf("the local work must survive in the trash: %v %q", rerr, b)
	}
	if st, _ := state.Load(); st.Repos["tools"] != nil {
		t.Error("the repository should be unregistered")
	}
}

// A clean clone's report must not cry wolf: no changes, no warning.
func TestUninstallDoesNotWarnAboutChangesACleanCloneDoesNotHave(t *testing.T) {
	startFakeHost(t)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})

	out, err := run(t, "", "uninstall", "tools")
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if strings.Contains(out, "uncommitted") {
		t.Errorf("a clean clone must not be flagged as holding changes:\n%s", out)
	}
}

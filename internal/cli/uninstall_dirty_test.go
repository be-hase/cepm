package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/state"
)

// uninstall deletes the clone, and local edits in it are treated as precious
// everywhere else (update skips dirty trees, stash-pop failures halt with
// recovery steps): they must not vanish without an explicit yes, and the
// headless refusal has to name the way out.
func TestUninstallRefusesADirtyCloneWithoutConsent(t *testing.T) {
	startFakeHost(t)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "init")
	if err := os.WriteFile(filepath.Join(dir, "edited.js"), []byte("local work"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = run(t, "", "uninstall", "tools")
	if err == nil {
		t.Fatal("a headless uninstall of a dirty clone must refuse")
	}
	if !strings.Contains(err.Error(), "--keep-files") {
		t.Errorf("the refusal should name --keep-files as the way out: %v", err)
	}
	if st, _ := state.Load(); st.Repos["tools"] == nil {
		t.Error("the repository must stay registered after the refusal")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Error("the clone must survive the refusal")
	}

	interactive(t)
	if _, err = run(t, "n\n", "uninstall", "tools"); err == nil {
		t.Fatal("declining the prompt must abort")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Error("the clone must survive a declined prompt")
	}

	if out, err := run(t, "y\n", "uninstall", "tools"); err != nil {
		t.Fatalf("consented uninstall failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("the clone should be deleted once the user consented")
	}
}

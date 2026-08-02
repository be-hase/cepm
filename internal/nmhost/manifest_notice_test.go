package nmhost

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/config"
	"github.com/be-hase/cepm/internal/extid"
	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/updater"
)

// A reload re-reads code and keeps the manifest Chrome cached. The periodic
// updater knows from the diff which extensions had their manifest.json
// changed, and dropping that on the way to the reload leaves a log saying
// "reloaded" for an update whose new permissions are not live.
func TestPeriodicUpdateSaysWhenTheManifestChanged(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "cepm-host")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("CEPM_HOME", dir)

	base := t.TempDir()
	bare := filepath.Join(base, "origin.git")
	hostGit(t, base, "init", "--bare", "--initial-branch=main", bare)
	author := filepath.Join(base, "author")
	hostGit(t, base, "clone", bare, author)
	writeManifest := func(root, version string) {
		if err := os.MkdirAll(filepath.Join(root, "ext"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "ext", "manifest.json"),
			[]byte(`{"manifest_version":3,"name":"Ext","version":"`+version+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest(author, "1.0")
	hostGit(t, author, "add", "-A")
	hostGit(t, author, "commit", "-m", "initial")
	hostGit(t, author, "push", "origin", "main")

	clone, err := updater.RepoDir("testrepo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
		t.Fatal(err)
	}
	hostGit(t, base, "clone", bare, clone)
	head := hostGit(t, clone, "rev-parse", "HEAD")
	id, err := extid.FromPath(filepath.Join(clone, "ext"))
	if err != nil {
		t.Fatal(err)
	}
	st := state.New()
	st.Repos["testrepo"] = &state.Repo{
		URL: bare, Track: state.TrackBranch, Branch: "main", Head: head,
		Extensions: []state.Extension{{Dir: "ext", Name: "Ext", ID: id}},
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	// The upstream change touches manifest.json and nothing else.
	writeManifest(author, "2.0")
	hostGit(t, author, "add", "-A")
	hostGit(t, author, "commit", "-m", "bump")
	hostGit(t, author, "push", "origin", "main")

	var buf syncBuffer
	h := &Host{log: slog.New(slog.NewTextHandler(&buf, nil))}
	h.autoUpdate(context.Background(), &config.Config{})

	logged := buf.String()
	if !strings.Contains(logged, "manifest.json changed") {
		t.Errorf("the manifest change must be reported, not swallowed:\n%s", logged)
	}
	if !strings.Contains(logged, id) {
		t.Errorf("the report must name the extension it applies to:\n%s", logged)
	}
}

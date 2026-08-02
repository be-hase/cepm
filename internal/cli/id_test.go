package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/extid"
)

// "cepm id" answers "what id will Chrome give this directory". Chrome
// resolves symlinks before it hashes the path, so an unresolved argument
// would confidently print an id that no extension will ever have — the worst
// kind of wrong answer for a command whose whole output is one identifier.
func TestIDResolvesSymlinksLikeChromeDoes(t *testing.T) {
	real := t.TempDir()
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	extDir := filepath.Join(resolved, "ext")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extDir, "manifest.json"),
		[]byte(`{"manifest_version":3,"name":"Ext","version":"1.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	want, err := extid.FromPath(extDir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "", "id", filepath.Join(link, "ext"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("id via a symlink = %s, want the resolved %s", got, want)
	}
}

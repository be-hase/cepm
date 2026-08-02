package nmmanifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/helperext"
	"github.com/be-hase/cepm/internal/paths"
)

// fakeHome points every home-derived path (NativeMessagingHostsDir reads
// $HOME / $XDG_CONFIG_HOME) at a throwaway directory, so no test can touch
// the developer's real Chrome configuration.
func fakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func TestNewFillsTheFieldsChromeRequires(t *testing.T) {
	m := New("/opt/cepm/bin/cepm-host")
	if m.Name != helperext.HostName {
		t.Errorf("name = %q, want %q", m.Name, helperext.HostName)
	}
	if m.Path != "/opt/cepm/bin/cepm-host" {
		t.Errorf("path = %q", m.Path)
	}
	if m.Type != "stdio" {
		t.Errorf("type = %q, want stdio", m.Type)
	}
	want := "chrome-extension://" + helperext.ExtensionID() + "/"
	if len(m.AllowedOrigins) != 1 || m.AllowedOrigins[0] != want {
		t.Errorf("allowed_origins = %v, want exactly [%s]", m.AllowedOrigins, want)
	}
}

func TestFileNameMatchesTheHostName(t *testing.T) {
	if got, want := FileName(), helperext.HostName+".json"; got != want {
		t.Errorf("FileName() = %q, want %q", got, want)
	}
}

func TestInstallReadRoundTrip(t *testing.T) {
	fakeHome(t)
	variant := paths.ChromeVariants[0]
	path, err := Install(variant, "/opt/cepm/bin/cepm-host")
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("installed manifest is not valid JSON: %v", err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("installed manifest should end with a newline")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("manifest mode = %o, want 644 (Chrome must be able to read it)", perm)
	}

	m, gotPath, err := Read(variant)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != path {
		t.Errorf("Read path = %q, want %q", gotPath, path)
	}
	if m.Path != "/opt/cepm/bin/cepm-host" || m.Name != helperext.HostName {
		t.Errorf("round trip mismatch: %+v", m)
	}
}

func TestReinstallReplacesTheManifest(t *testing.T) {
	fakeHome(t)
	variant := paths.ChromeVariants[0]
	if _, err := Install(variant, "/old/cepm-host"); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(variant, "/new/cepm-host"); err != nil {
		t.Fatal(err)
	}
	m, _, err := Read(variant)
	if err != nil {
		t.Fatal(err)
	}
	if m.Path != "/new/cepm-host" {
		t.Errorf("reinstall should replace the path, got %q", m.Path)
	}
}

// A failure before the rename must leave the previous, working manifest in
// place: a half-installed manifest means Chrome cannot launch the host at
// all, which is strictly worse than an outdated one.
func TestFailedWriteKeepsTheExistingManifest(t *testing.T) {
	fakeHome(t)
	variant := paths.ChromeVariants[0]
	if _, err := Install(variant, "/old/cepm-host"); err != nil {
		t.Fatal(err)
	}

	orig := createTemp
	createTemp = func(dir, pattern string) (*os.File, error) {
		return nil, fmt.Errorf("injected write failure")
	}
	defer func() { createTemp = orig }()

	if _, err := Install(variant, "/new/cepm-host"); err == nil {
		t.Fatal("Install should report the injected failure")
	}
	m, _, err := Read(variant)
	if err != nil {
		t.Fatalf("the previous manifest should still be readable: %v", err)
	}
	if m.Path != "/old/cepm-host" {
		t.Errorf("the previous manifest should be untouched, got path %q", m.Path)
	}
}

func TestRemoveOthersKeepsOnlyTheNamedVariant(t *testing.T) {
	if len(paths.ChromeVariants) < 2 {
		t.Skip("only one Chrome variant on this platform")
	}
	fakeHome(t)
	keep, other := paths.ChromeVariants[0], paths.ChromeVariants[1]
	if _, err := Install(keep, "/x/cepm-host"); err != nil {
		t.Fatal(err)
	}
	otherPath, err := Install(other, "/x/cepm-host")
	if err != nil {
		t.Fatal(err)
	}

	removed, err := RemoveOthers(keep)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != otherPath {
		t.Errorf("removed = %v, want exactly [%s]", removed, otherPath)
	}
	if _, _, err := Read(keep); err != nil {
		t.Errorf("the kept variant should survive: %v", err)
	}
	if _, err := os.Stat(otherPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the other variant's manifest should be gone, got %v", err)
	}
}

func TestUnknownVariantIsRejected(t *testing.T) {
	fakeHome(t)
	if _, err := Install("netscape", "/x/cepm-host"); err == nil {
		t.Error("Install must reject an unknown variant")
	}
	if _, _, err := Read("netscape"); err == nil {
		t.Error("Read must reject an unknown variant")
	}
}

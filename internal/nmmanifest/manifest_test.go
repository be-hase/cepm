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
func TestFailedTempCreationKeepsTheExistingManifest(t *testing.T) {
	fakeHome(t)
	variant := paths.ChromeVariants[0]
	if _, err := Install(variant, "/old/cepm-host"); err != nil {
		t.Fatal(err)
	}

	orig := createTemp
	createTemp = func(dir, pattern string) (*os.File, error) {
		return nil, fmt.Errorf("injected temp-creation failure")
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

// The same guarantee when the temp file exists but writing to it fails —
// this is the path where a missing error check would rename an empty file
// over the working manifest. The injected file is opened read-only, so only
// the Write fails; chmod and close still succeed.
func TestFailedWriteKeepsTheExistingManifest(t *testing.T) {
	fakeHome(t)
	variant := paths.ChromeVariants[0]
	path, err := Install(variant, "/old/cepm-host")
	if err != nil {
		t.Fatal(err)
	}

	orig := createTemp
	createTemp = func(dir, pattern string) (*os.File, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		name := f.Name()
		if err := f.Close(); err != nil {
			return nil, err
		}
		return os.OpenFile(name, os.O_RDONLY, 0)
	}
	defer func() { createTemp = orig }()

	_, err = Install(variant, "/new/cepm-host")
	if err == nil {
		t.Fatal("Install should report the failed write")
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "re-run cepm setup") {
		t.Errorf("the error should name the manifest path and the way out, got: %v", err)
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

	removed, failed := RemoveOthers(keep)
	if len(failed) != 0 {
		t.Fatalf("nothing should have failed: %+v", failed)
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

// Every leftover manifest is a Chrome that can still start a host, so one
// that cannot be removed must not stop the attempt on the rest — and must
// be reported rather than swallowed. Stopping at the first failure would
// leave variants nobody even tried.
func TestRemoveOthersTriesEveryVariantAndReportsWhatItCouldNot(t *testing.T) {
	if len(paths.ChromeVariants) < 3 {
		t.Skipf("need three Chrome variants to have one after the blocked one, have %v", paths.ChromeVariants)
	}
	fakeHome(t)
	keep := paths.ChromeVariants[0]
	blocked, alsoOther := paths.ChromeVariants[1], paths.ChromeVariants[2]
	if _, err := Install(keep, "/x/cepm-host"); err != nil {
		t.Fatal(err)
	}
	blockedPath, err := Install(blocked, "/x/cepm-host")
	if err != nil {
		t.Fatal(err)
	}
	alsoOtherPath, err := Install(alsoOther, "/x/cepm-host")
	if err != nil {
		t.Fatal(err)
	}
	// A directory the user cannot write is how this happens in practice.
	blockedDir := filepath.Dir(blockedPath)
	if err := os.Chmod(blockedDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blockedDir, 0o700) })

	removed, failed := RemoveOthers(keep)
	if len(failed) != 1 || failed[0].Path != blockedPath {
		t.Fatalf("the manifest that survived must be named: removed=%v failed=%+v", removed, failed)
	}
	if len(removed) != 1 || removed[0] != alsoOtherPath {
		t.Errorf("the variant after the blocked one must still be attempted: removed=%v", removed)
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

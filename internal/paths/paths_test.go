package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// The clones are internal extension source and the logs describe them: a lax
// home directory permission must not expose ~/.cepm to other local users.
// EnsureLayout also tightens directories that already exist too loose.
func TestEnsureLayoutIsOwnerOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CEPM_HOME", home)
	// Pre-create one directory too open, as if by an older build or by hand.
	if err := os.MkdirAll(filepath.Join(home, "repos"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := EnsureLayout(); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"", "repos", "helper", "bin", "run", "logs"} {
		p := filepath.Join(home, sub)
		st, err := os.Stat(p)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if perm := st.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s mode = %o, want 0700", p, perm)
		}
	}
}

// Extension ids are hashes of the directory Chrome loaded from, and Chrome
// resolves symlinks before hashing. A symlinked ~/.cepm (or CEPM_HOME) must
// therefore never reach an id calculation unresolved: cepm would compute an
// id Chrome does not have, and — because Validate re-derives it the same way
// — would keep agreeing with itself while every reload missed.
func TestCepmDirIsResolvedBeforeAnyIDIsDerived(t *testing.T) {
	real := t.TempDir()
	// t.TempDir() is itself under a symlink on macOS (/var -> /private/var),
	// so compare against the resolved form, not the string handed out.
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "cepm-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("CEPM_HOME", link)

	got, err := CepmDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != resolved {
		t.Errorf("CepmDir() = %q, want the resolved %q", got, resolved)
	}
	// The derived locations have to inherit it: they are what the ids are
	// built from.
	repos, err := ReposDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(resolved, "repos"); repos != want {
		t.Errorf("ReposDir() = %q, want %q", repos, want)
	}
}

// The paths under the home are the ones cepm operates on as filesystem
// entries — reset renames repos/ into a backup, uninstall deletes a clone —
// and there a symlink is the thing to act on, not something to see through.
// Resolving would make reset move the target and leave the link dangling, or
// fail outright across devices. Extension ids need the opposite, and get it
// in extid, which canonicalizes what it is given.
func TestPathsUnderTheHomeKeepTheirSymlinks(t *testing.T) {
	home := t.TempDir()
	elsewhere := t.TempDir()
	t.Setenv("CEPM_HOME", home)
	if err := os.Symlink(elsewhere, filepath.Join(home, "repos")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}

	got, err := ReposDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(resolvedHome, "repos"); got != want {
		t.Errorf("ReposDir() = %q, want the symlink itself at %q", got, want)
	}
}

// A clone's id is derived before the clone exists, and stays derivable after
// the user deletes it, so Canonical resolves as far as the path exists and
// appends the rest rather than failing.
func TestCanonicalResolvesThePrefixThatExists(t *testing.T) {
	real := t.TempDir()
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := Canonical(filepath.Join(link, "repos", "tools", "ext"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(resolved, "repos", "tools", "ext"); got != want {
		t.Errorf("Canonical() = %q, want %q", got, want)
	}
}

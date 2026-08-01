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

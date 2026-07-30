package launcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/paths"
)

func TestInstallAndRecordedPath(t *testing.T) {
	t.Setenv("CEPM_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir()) // no shims present

	if err := Install("/opt/homebrew/bin/cepm"); err != nil {
		t.Fatal(err)
	}
	got, err := RecordedPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/opt/homebrew/bin/cepm" {
		t.Errorf("RecordedPath = %q", got)
	}

	path, _ := paths.LauncherPath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	if !strings.HasPrefix(script, "#!/bin/sh") {
		t.Error("launcher must be a shell script")
	}
	if !strings.Contains(script, `exec "$CEPM" native-host "$@"`) {
		t.Errorf("launcher must exec native-host:\n%s", script)
	}
	if strings.Contains(script, "[ -x") {
		t.Errorf("no shim fallback expected when no shims exist:\n%s", script)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Error("launcher must be executable")
	}
}

func TestInstallAddsShimFallback(t *testing.T) {
	t.Setenv("CEPM_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("HOME", home)
	shim := filepath.Join(home, ".local", "share", "mise", "shims", "cepm")
	if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shim, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Install("/somewhere/versioned/cepm"); err != nil {
		t.Fatal(err)
	}
	path, _ := paths.LauncherPath()
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), shim) {
		t.Errorf("launcher should fall back to the mise shim:\n%s", data)
	}
}

func TestRecordedPathMissingLauncher(t *testing.T) {
	t.Setenv("CEPM_HOME", t.TempDir())
	got, err := RecordedPath()
	if err != nil || got != "" {
		t.Errorf("missing launcher should be (\"\", nil), got (%q, %v)", got, err)
	}
}

func TestSelfHeal(t *testing.T) {
	t.Setenv("CEPM_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	// Before setup: SelfHeal must do nothing.
	SelfHeal()
	if got, _ := RecordedPath(); got != "" {
		t.Fatalf("SelfHeal created a launcher before setup: %q", got)
	}

	// Simulate a stale recorded path; SelfHeal should rewrite it to the
	// current executable.
	if err := Install("/stale/path/cepm"); err != nil {
		t.Fatal(err)
	}
	SelfHeal()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := RecordedPath(); got != exe {
		t.Errorf("SelfHeal recorded %q, want current executable %q", got, exe)
	}
}

package launcher

import (
	"os"
	"os/exec"
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

// The launcher is a shell script that Chrome executes, so the recorded path
// must survive as data — never as something the shell evaluates.
func TestInstallQuotesHostilePaths(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	hostile := []string{
		"/tmp/a b/cepm",
		"/tmp/a$(touch " + marker + ")/cepm",
		"/tmp/a`touch " + marker + "`/cepm",
		`/tmp/it's/cepm`,
		`/tmp/a"b/cepm`,
		"/tmp/a${HOME}/cepm",
	}
	for _, p := range hostile {
		t.Setenv("CEPM_HOME", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		if err := Install(p); err != nil {
			t.Fatalf("Install(%q): %v", p, err)
		}
		got, err := RecordedPath()
		if err != nil {
			t.Fatalf("RecordedPath after Install(%q): %v", p, err)
		}
		if got != p {
			t.Errorf("round trip changed the path: got %q, want %q", got, p)
		}

		// Ask a real shell what the script resolves $CEPM to. The final exec
		// line is dropped so the shell only performs the assignments.
		launcherPath, err := paths.LauncherPath()
		if err != nil {
			t.Fatal(err)
		}
		script, err := os.ReadFile(launcherPath)
		if err != nil {
			t.Fatal(err)
		}
		var assignments []string
		for _, line := range strings.Split(string(script), "\n") {
			if !strings.HasPrefix(line, "exec ") {
				assignments = append(assignments, line)
			}
		}
		probe := filepath.Join(t.TempDir(), "probe.sh")
		if err := os.WriteFile(probe, []byte(strings.Join(assignments, "\n")), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command("sh", "-c", ". "+shellQuote(probe)+`; printf %s "$CEPM"`).Output()
		if err != nil {
			t.Fatalf("sourcing the launcher for %q: %v", p, err)
		}
		if string(out) != p {
			t.Errorf("shell resolved CEPM to %q, want %q", out, p)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("path %q caused command execution (created %s)", p, marker)
		}
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

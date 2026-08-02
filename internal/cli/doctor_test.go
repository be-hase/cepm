package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/nmmanifest"
	"github.com/be-hase/cepm/internal/paths"
	"github.com/be-hase/cepm/internal/state"
)

// An extension enabled in cepm but switched off in chrome://extensions is
// the one state where pulls happen but reloads never do (updates respect the
// Chrome-side switch). Without a diagnostic, "updates don't work" doctor
// output reads all green.
func TestDoctorFlagsExtensionDisabledInChrome(t *testing.T) {
	h := startFakeHost(t, idA)
	h.disabledInChrome = map[string]bool{idA: true}
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})

	out, _ := run(t, "", "doctor")
	if !strings.Contains(out, "turned off in chrome://extensions") {
		t.Errorf("doctor should flag the Chrome-side disable:\n%s", out)
	}

	// And not when Chrome agrees the extension is on.
	h.mu.Lock()
	h.disabledInChrome = nil
	h.mu.Unlock()
	out, _ = run(t, "", "doctor")
	if strings.Contains(out, "turned off in chrome://extensions") {
		t.Errorf("doctor must not flag an extension Chrome has enabled:\n%s", out)
	}
}

// Chrome matches the manifest by name and launches by type: wrong values
// mean connectNative silently never reaches the host. doctor must flag every
// field Chrome depends on, not just origins and path.
func TestDoctorFlagsEveryBrokenNMManifestField(t *testing.T) {
	startFakeHost(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	variant := paths.ChromeVariants[0]
	launcherPath, err := paths.LauncherPath()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := paths.NativeMessagingHostsDir(variant)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeManifest := func(m nmmanifest.HostManifest) {
		t.Helper()
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, nmmanifest.FileName()), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		label   string
		corrupt func(*nmmanifest.HostManifest)
		flagged string // substring doctor must print for this corruption
	}{
		{"name", func(m *nmmanifest.HostManifest) { m.Name = "evil.example.host" },
			"Chrome will not match"},
		{"type", func(m *nmmanifest.HostManifest) { m.Type = "" },
			`want "stdio"`},
		{"origins", func(m *nmmanifest.HostManifest) {
			m.AllowedOrigins = append(m.AllowedOrigins, "chrome-extension://aaaabbbbccccddddeeeeffffgggghhhh/")
		}, "allowed_origins should list only"},
		{"description", func(m *nmmanifest.HostManifest) { m.Description = "  " },
			"description is empty"},
		{"empty path", func(m *nmmanifest.HostManifest) { m.Path = "" },
			"has no path"},
		{"relative path", func(m *nmmanifest.HostManifest) { m.Path = "bin/cepm-host" },
			"is not absolute"},
		{"wrong absolute path", func(m *nmmanifest.HostManifest) { m.Path = "/somewhere/else/cepm-host" },
			"instead of the launcher"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			m := nmmanifest.New(launcherPath)
			tc.corrupt(&m)
			writeManifest(m)
			out, _ := run(t, "", "doctor")
			if !strings.Contains(out, tc.flagged) {
				t.Errorf("doctor should flag the broken %s (want %q in output):\n%s", tc.label, tc.flagged, out)
			}
		})
	}

	// And a healthy manifest raises none of those flags.
	writeManifest(nmmanifest.New(launcherPath))
	out, _ := run(t, "", "doctor")
	for _, tc := range cases {
		if strings.Contains(out, tc.flagged) {
			t.Errorf("doctor flags a healthy manifest (%q):\n%s", tc.flagged, out)
		}
	}
}

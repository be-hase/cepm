package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/helperext"
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

// An unusable config.toml pauses the host's periodic updates, so doctor —
// where "updates stopped working" gets diagnosed — must name the error
// instead of reading all green, and must show the effective values when the
// file is healthy or absent.
func TestDoctorFlagsABrokenConfig(t *testing.T) {
	startFakeHost(t)
	cfgPath := filepath.Join(os.Getenv("CEPM_HOME"), "config.toml")

	// No file at all: the defaults are what actually runs; show them as ok.
	out, _ := run(t, "", "doctor")
	if !strings.Contains(out, "auto=true") || !strings.Contains(out, "interval=") || !strings.Contains(out, "stash_dirty=") {
		t.Errorf("doctor should show the effective defaults without a config file:\n%s", out)
	}

	for label, broken := range map[string]string{
		"parse error":        "[update\ninterval=",
		"unknown key":        "[update]\natuo = false\n",
		"invalid duration":   "[update]\ninterval = \"soon\"\n",
		"too-short interval": "[update]\ninterval = \"1s\"\n",
	} {
		if err := os.WriteFile(cfgPath, []byte(broken), 0o644); err != nil {
			t.Fatal(err)
		}
		out, _ := run(t, "", "doctor")
		if !strings.Contains(out, "fix the file") {
			t.Errorf("doctor should flag the config with a %s:\n%s", label, out)
		}
	}

	// The same diagnostic reaches --json consumers.
	if out, _ := run(t, "", "doctor", "--json"); !strings.Contains(out, "fix the file") {
		t.Errorf("doctor --json should carry the config failure:\n%s", out)
	}

	// auto=false with non-default interval and stash_dirty: all three
	// effective values must show — the off state is exactly when someone
	// wonders what would apply after turning it back on.
	if err := os.WriteFile(cfgPath,
		[]byte("[update]\nauto = false\ninterval = \"2h\"\n[git]\nstash_dirty = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, "", "doctor")
	if strings.Contains(out, "fix the file") {
		t.Errorf("doctor flags a healthy config:\n%s", out)
	}
	for _, want := range []string{"auto=false", "interval=2h0m0s", "stash_dirty=true"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor should show %q even with auto off:\n%s", want, out)
		}
	}

	// The JSON output carries the identical diagnostic, decoded, not just
	// substring-matched.
	jsonOut, _ := run(t, "", "doctor", "--json")
	var diags []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &diags); err != nil {
		t.Fatalf("doctor --json is not valid JSON: %v\n%s", err, jsonOut)
	}
	found := false
	for _, d := range diags {
		if d.Name != "config" {
			continue
		}
		found = true
		if d.Status != "ok" {
			t.Errorf("config diagnostic status = %q, want ok", d.Status)
		}
		for _, want := range []string{"auto=false", "interval=2h0m0s", "stash_dirty=true"} {
			if !strings.Contains(d.Detail, want) {
				t.Errorf("config diagnostic detail should carry %q, got %q", want, d.Detail)
			}
		}
	}
	if !found {
		t.Errorf("doctor --json has no config diagnostic:\n%s", jsonOut)
	}
}

// The host refuses to drive an incompatible helper, so doctor must say so —
// with both versions and the fix — instead of showing a healthy connection.
func TestDoctorFlagsAnIncompatibleHelper(t *testing.T) {
	h := startFakeHost(t)
	h.helperVersion = "0.0.1"
	h.helperTooOld = true

	out, _ := run(t, "", "doctor")
	for _, want := range []string{"helper v0.0.1", "older than this host supports", "restart Chrome"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor should report the incompatibility (%q missing):\n%s", want, out)
		}
	}
	if jsonOut, _ := run(t, "", "doctor", "--json"); !strings.Contains(jsonOut, "older than this host supports") {
		t.Errorf("doctor --json should carry the incompatibility:\n%s", jsonOut)
	}

	// A compatible helper stays a healthy row.
	h.mu.Lock()
	h.helperTooOld = false
	h.mu.Unlock()
	out, _ = run(t, "", "doctor")
	if strings.Contains(out, "older than this host supports") {
		t.Errorf("doctor flags a compatible helper:\n%s", out)
	}
}

// A current marker next to corrupted helper files means Chrome may be
// running something else entirely under cepm's name; doctor and setup must
// both notice content, not just the marker.
func TestDoctorAndSetupNoticeCorruptedHelperFiles(t *testing.T) {
	startFakeHost(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if out, err := run(t, "", "setup"); err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	helperDir, err := paths.HelperDir()
	if err != nil {
		t.Fatal(err)
	}
	bg := filepath.Join(helperDir, "background.js")
	if err := os.WriteFile(bg, []byte("// corrupted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _ := run(t, "", "doctor")
	if !strings.Contains(out, "do not match this binary") {
		t.Errorf("doctor should flag corrupted helper files:\n%s", out)
	}

	if out, err := run(t, "", "setup"); err != nil {
		t.Fatalf("setup after corruption: %v\n%s", err, out)
	}
	if !helperext.InstalledMatches(helperDir) {
		t.Error("setup should regenerate corrupted helper files")
	}
	out, _ = run(t, "", "doctor")
	if strings.Contains(out, "do not match this binary") {
		t.Errorf("doctor flags a repaired helper:\n%s", out)
	}
}

// A running host from before compatibility and protocol reporting sends
// zero-valued new fields; that is "no verdict", never "helper too old" — and
// the generation difference itself gets its own diagnostic.
func TestDoctorHandlesAHostThatPredatesCompatReporting(t *testing.T) {
	h := startFakeHost(t)
	h.oldHostInfo = true
	h.helperVersion = "0.1.0" // a perfectly healthy helper

	out, _ := run(t, "", "doctor")
	if strings.Contains(out, "older than this host supports") {
		t.Errorf("an absent verdict must not read as an incompatible helper:\n%s", out)
	}
	if !strings.Contains(out, "restart Chrome to relaunch the updated host") {
		t.Errorf("the generation difference should be diagnosed:\n%s", out)
	}
}

// The host version is displayed verbatim: releases already carry the v
// prefix and dev builds say "dev" — a forced prefix printed "vv...".
func TestDoctorShowsHostVersionVerbatim(t *testing.T) {
	h := startFakeHost(t)
	h.hostVersion = "v1.2.3"
	out, _ := run(t, "", "doctor")
	if strings.Contains(out, "vv1.2.3") {
		t.Errorf("host version rendered with a double v:\n%s", out)
	}
	if !strings.Contains(out, "v1.2.3 connected") {
		t.Errorf("host version should appear verbatim:\n%s", out)
	}

	h.mu.Lock()
	h.hostVersion = "dev"
	h.mu.Unlock()
	out, _ = run(t, "", "doctor")
	if !strings.Contains(out, "dev connected") {
		t.Errorf("a dev version should appear as-is:\n%s", out)
	}
}

// A host of another generation refuses every operation, so doctor must fail
// (exit non-zero), not merely warn: "nothing works" is not a warning.
func TestDoctorFailsOnAProtocolMismatch(t *testing.T) {
	h := startFakeHost(t)
	h.oldHostInfo = true // protocol 0

	out, err := run(t, "", "doctor")
	if err == nil {
		t.Errorf("doctor must exit non-zero when the host generation differs:\n%s", out)
	}
	var diags []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	jsonOut, _ := run(t, "", "doctor", "--json")
	if uerr := json.Unmarshal([]byte(jsonOut), &diags); uerr != nil {
		t.Fatalf("doctor --json is not valid JSON: %v", uerr)
	}
	found := false
	for _, d := range diags {
		if d.Name == "host generation" {
			found = true
			if d.Status != "fail" {
				t.Errorf("host generation status = %q, want fail", d.Status)
			}
		}
	}
	if !found {
		t.Errorf("doctor --json has no host generation diagnostic:\n%s", jsonOut)
	}
}

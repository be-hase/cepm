package helperext

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The helper's extension ID must never change: the native messaging manifest,
// docs, and every user's loaded helper depend on it.
const fixedID = "mdnfnogffnkigldddmnmfganbalgaggb"

func TestExtensionIDIsFixed(t *testing.T) {
	if got := ExtensionID(); got != fixedID {
		t.Errorf("ExtensionID() = %q, want %q (did the embedded key change?)", got, fixedID)
	}
}

func TestInstall(t *testing.T) {
	dir := t.TempDir()
	if err := Install(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		ManifestVersion int      `json:"manifest_version"`
		Version         string   `json:"version"`
		Key             string   `json:"key"`
		Permissions     []string `json:"permissions"`
		Background      struct {
			ServiceWorker string `json:"service_worker"`
		} `json:"background"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("generated manifest is not valid JSON: %v", err)
	}
	if m.ManifestVersion != 3 || m.Version != Version || m.Key != PublicKeyBase64 {
		t.Errorf("unexpected manifest: %+v", m)
	}
	if m.Background.ServiceWorker != "background.js" {
		t.Errorf("service worker = %q", m.Background.ServiceWorker)
	}
	perms := map[string]bool{}
	for _, p := range m.Permissions {
		perms[p] = true
	}
	if !perms["management"] || !perms["nativeMessaging"] || !perms["alarms"] || !perms["storage"] {
		t.Errorf("missing permissions: %v", m.Permissions)
	}
	if _, err := os.Stat(filepath.Join(dir, "background.js")); err != nil {
		t.Error("background.js not installed")
	}
	if v := InstalledVersion(dir); v != Version {
		t.Errorf("InstalledVersion = %q, want %q", v, Version)
	}
}

// The version marker alone is not evidence: a truncated or hand-edited file
// next to a current marker must read as "does not match".
func TestInstalledMatchesDetectsCorruption(t *testing.T) {
	dir := t.TempDir()
	if err := Install(dir); err != nil {
		t.Fatal(err)
	}
	if !InstalledMatches(dir) {
		t.Fatal("a fresh install must match")
	}

	bg := filepath.Join(dir, "background.js")
	orig, err := os.ReadFile(bg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bg, []byte("// not the helper\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if InstalledMatches(dir) {
		t.Error("a corrupted (non-empty) background.js must not match")
	}
	if err := os.WriteFile(bg, orig, 0o644); err != nil {
		t.Fatal(err)
	}

	// And a manifest whose permissions were edited.
	mf := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(mf)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(data), `"storage"`, `"tabs"`, 1)
	if edited == string(data) {
		t.Fatal("fixture missed: manifest should mention storage")
	}
	if err := os.WriteFile(mf, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if InstalledMatches(dir) {
		t.Error("an edited manifest must not match")
	}
}

// The READMEs promise which permissions the helper requests; the list must
// track the generated manifest, in both languages.
func TestReadmesListEveryHelperPermission(t *testing.T) {
	files, err := renderFiles()
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Permissions []string `json:"permissions"`
	}
	for _, f := range files {
		if f.name == "manifest.json" {
			if err := json.Unmarshal(f.content, &m); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(m.Permissions) == 0 {
		t.Fatal("no permissions found in the generated manifest")
	}
	for _, readme := range []string{
		filepath.Join("..", "..", "README.md"),
		filepath.Join("..", "..", "docs", "README.ja.md"),
	} {
		b, err := os.ReadFile(readme)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range m.Permissions {
			if !strings.Contains(string(b), "`"+p+"`") {
				t.Errorf("%s does not document the %q permission", readme, p)
			}
		}
	}
}

// The protocol the helper announces has to come from the Go constant, so
// the generated file and the host cannot disagree about what "current"
// means. A refresh that failed halfway is exactly when they would.
func TestGeneratedHelperCarriesTheProtocol(t *testing.T) {
	dir := t.TempDir()
	if err := Install(dir); err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile(filepath.Join(dir, "background.js"))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("const HELPER_PROTOCOL = %d;", Protocol)
	if !strings.Contains(string(js), want) {
		t.Errorf("generated background.js does not carry %q", want)
	}
	if strings.Contains(string(js), protocolPlaceholder) {
		t.Error("the placeholder survived into the generated file")
	}
	// Carrying the constant is not enough: the hello has to send it, and
	// the host has to be told which field. Go tests cannot run this JS, so
	// the wiring is pinned by contract.
	if !strings.Contains(string(js), "protocol: HELPER_PROTOCOL") {
		t.Error("the hello no longer sends the protocol; the host would read a helper of unknown generation")
	}
}

// Two invariants in the helper have no Go-side expression and no way to be
// exercised from here — running this JS would take a browser, and the races
// they close need a service worker to die at a chosen instant. They are
// pinned by contract instead, so removing them is at least loud.
//
//   - Recovery at worker start and a reload arriving on the port must not
//     interleave on one id: recovery would otherwise delete the marker a
//     live reload had just set, and a worker death there leaves the
//     extension disabled with nothing recording that cepm disabled it.
//   - The uninstall confirmation dialog must report that it is still open,
//     or the host and CLI go back to giving up on a dialog that is still on
//     screen while holding the update lock open for it.
func TestGeneratedHelperKeepsTheInvariantsGoCannotTest(t *testing.T) {
	dir := t.TempDir()
	if err := Install(dir); err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile(filepath.Join(dir, "background.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ snippet, why string }{
		{"function withExtension(", "the per-id serialization is gone"},
		{"withExtension(id, () => reloadOneLocked(id))", "reloads no longer serialize against recovery"},
		{"await withExtension(id, async () => {", "recovery no longer serializes against reloads"},
		{"if (!(await isInflight(id))) return;", "recovery no longer re-checks the marker it is about to delete"},
		{`post({ type: "uninstallPending"`, "the uninstall dialog no longer reports that it is open"},
	} {
		if !strings.Contains(string(js), want.snippet) {
			t.Errorf("%s (looked for %q)", want.why, want.snippet)
		}
	}
}

package scan

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeManifest(t *testing.T, dir, name string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "manifest.json"),
		`{"manifest_version": 3, "name": "`+name+`", "version": "1.0"}`)
}

func TestDetectWalk(t *testing.T) {
	repo := t.TempDir()
	writeManifest(t, filepath.Join(repo, "ext-b"), "Ext B")
	writeManifest(t, filepath.Join(repo, "tools", "ext-a"), "Ext A")
	// Should all be ignored:
	writeManifest(t, filepath.Join(repo, "node_modules", "fake"), "NPM Fake")
	writeManifest(t, filepath.Join(repo, ".hidden", "ext"), "Hidden")
	writeManifest(t, filepath.Join(repo, "ext-b", "nested"), "Nested inside ext")
	writeFile(t, filepath.Join(repo, "docs", "manifest.json"), `{"name": "not an extension"}`)

	got, err := Detect(repo)
	if err != nil {
		t.Fatal(err)
	}
	want := []Extension{
		{Dir: "ext-b", Name: "Ext B"},
		{Dir: filepath.Join("tools", "ext-a"), Name: "Ext A"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Detect = %+v, want %+v", got, want)
	}
}

// A manifest.json that exists but does not parse means the tree is in a bad
// state (a conflicted stash pop, a partial checkout). Silently treating it as
// "not an extension" would unregister a working extension and let cleanup
// uninstall it from Chrome, so scanning must fail loudly instead.
func TestDetectFailsOnBrokenManifest(t *testing.T) {
	repo := t.TempDir()
	writeManifest(t, filepath.Join(repo, "good"), "Good")
	writeFile(t, filepath.Join(repo, "broken", "manifest.json"), `{invalid json`)

	if _, err := Detect(repo); err == nil {
		t.Fatal("Detect should fail when a manifest.json cannot be parsed")
	}

	// A conflicted merge leaves markers in the file; same requirement.
	writeFile(t, filepath.Join(repo, "broken", "manifest.json"),
		"<<<<<<< Updated upstream\n{\"manifest_version\":3,\"name\":\"A\"}\n=======\n{}\n>>>>>>> Stashed changes\n")
	if _, err := Detect(repo); err == nil {
		t.Fatal("Detect should fail on a conflicted manifest.json")
	}
}

func TestDetectRepoRootExtension(t *testing.T) {
	repo := t.TempDir()
	writeManifest(t, repo, "Root Ext")
	got, err := Detect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Dir != "." || got[0].Name != "Root Ext" {
		t.Errorf("Detect = %+v, want single root extension", got)
	}
}

func TestDetectManifestV2(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "old", "manifest.json"),
		`{"manifest_version": 2, "name": "Old Ext", "version": "1.0"}`)
	got, err := Detect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "Old Ext" {
		t.Errorf("Detect = %+v, want MV2 extension detected", got)
	}
}

func TestDetectI18nNameFallsBackToDirName(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "intl-ext", "manifest.json"),
		`{"manifest_version": 3, "name": "__MSG_appName__", "version": "1.0"}`)
	got, err := Detect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "intl-ext" {
		t.Errorf("Detect = %+v, want name fallback to dir name", got)
	}
}

func TestDetectWithRepoConfig(t *testing.T) {
	repo := t.TempDir()
	writeManifest(t, filepath.Join(repo, "dist", "ext-a"), "Dist A")
	writeManifest(t, filepath.Join(repo, "src", "ext-a"), "Src A") // not listed → ignored
	writeFile(t, filepath.Join(repo, "cepm.toml"), `extensions = ["dist/ext-a"]`)

	got, err := Detect(repo)
	if err != nil {
		t.Fatal(err)
	}
	want := []Extension{{Dir: filepath.Join("dist", "ext-a"), Name: "Dist A"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Detect = %+v, want %+v", got, want)
	}
}

func TestDetectConfigMissingManifest(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "cepm.toml"), `extensions = ["nope"]`)
	if _, err := Detect(repo); err == nil {
		t.Error("expected error for configured dir without manifest.json")
	}
}

func TestDetectConfigEscapingPath(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "cepm.toml"), `extensions = ["../outside"]`)
	if _, err := Detect(repo); err == nil {
		t.Error("expected error for extension dir outside the repo")
	}
}

// A repository controls its manifest, and cepm prints the name into tables
// and into the numbered menu people answer. Control characters would let a
// repo forge a row and misdirect which extension someone enables.
func TestDetectSanitizesNames(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "evil", "manifest.json"),
		"{\"manifest_version\":3,\"version\":\"1.0\",\"name\":\"Innocent\\u001b[2K\\rHACKED\\nBogus\\tzzz\"}")

	got, err := Detect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("unexpected result: %+v", got)
	}
	for _, r := range got[0].Name {
		if r == '\n' || r == '\r' || r == 0x1b {
			t.Fatalf("name still contains control characters: %q", got[0].Name)
		}
	}

	writeFile(t, filepath.Join(repo, "evil", "manifest.json"),
		`{"manifest_version":3,"version":"1.0","name":"`+strings.Repeat("x", 200)+`"}`)
	got, err = Detect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(got[0].Name)) > 45 {
		t.Errorf("name should be capped at Chrome's 45 characters, got %d", len([]rune(got[0].Name)))
	}
}

func TestDetectRejectsEscapingConfig(t *testing.T) {
	// Absolute paths and multi-level traversal, not just "..".
	for _, dir := range []string{"/etc", "a/../../..", "../outside"} {
		repo := t.TempDir()
		writeFile(t, filepath.Join(repo, "cepm.toml"), `extensions = ["`+dir+`"]`)
		if _, err := Detect(repo); err == nil {
			t.Errorf("Detect should reject extension dir %q", dir)
		}
	}

	// A symlink escapes filepath.Clean, so the resolved path must be checked.
	outside := t.TempDir()
	writeManifest(t, outside, "Outside")
	repo := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repo, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeFile(t, filepath.Join(repo, "cepm.toml"), `extensions = ["link"]`)
	if _, err := Detect(repo); err == nil {
		t.Error("Detect should reject an extension dir that resolves outside the repo")
	}
}

// tag_pattern reaches "git tag"; a value starting with "-" would be read as
// an option and let the repo dictate which revision cepm follows.
func TestLoadRepoConfigRejectsOptionLikeTagPattern(t *testing.T) {
	for _, p := range []string{"--format=%(refname)", "-v", "v*; rm -rf /", "v*\n--sort=x"} {
		repo := t.TempDir()
		writeFile(t, filepath.Join(repo, "cepm.toml"), "tag_pattern = "+strconv.Quote(p))
		if _, err := LoadRepoConfig(repo); err == nil {
			t.Errorf("tag_pattern %q should be rejected", p)
		}
	}
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "cepm.toml"), `tag_pattern = "release-v*"`)
	if _, err := LoadRepoConfig(repo); err != nil {
		t.Errorf("a normal glob should be accepted: %v", err)
	}
}

func TestLoadRepoConfig(t *testing.T) {
	repo := t.TempDir()
	if cfg, err := LoadRepoConfig(repo); err != nil || cfg != nil {
		t.Errorf("missing cepm.toml should return (nil, nil), got (%+v, %v)", cfg, err)
	}
	writeFile(t, filepath.Join(repo, "cepm.toml"), "track = \"tag\"\ntag_pattern = \"release-*\"\n")
	cfg, err := LoadRepoConfig(repo)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Track != "tag" || cfg.TagPattern != "release-*" {
		t.Errorf("unexpected config: %+v", cfg)
	}
	writeFile(t, filepath.Join(repo, "cepm.toml"), `track = "bogus"`)
	if _, err := LoadRepoConfig(repo); err == nil {
		t.Error("expected error for invalid track value")
	}
}

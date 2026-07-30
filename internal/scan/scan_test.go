package scan

import (
	"os"
	"path/filepath"
	"reflect"
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
	writeFile(t, filepath.Join(repo, "broken", "manifest.json"), `{invalid json`)

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

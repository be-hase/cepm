package helperext

import (
	"encoding/json"
	"os"
	"path/filepath"
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

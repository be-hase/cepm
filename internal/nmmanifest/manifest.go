// Package nmmanifest generates and installs Chrome's native messaging host
// manifest pointing at the cepm binary.
package nmmanifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/be-hase/cepm/internal/helperext"
	"github.com/be-hase/cepm/internal/paths"
)

// HostManifest is the JSON Chrome reads from NativeMessagingHosts/.
type HostManifest struct {
	Name           string   `json:"name"`
	Description    string   `json:"description"`
	Path           string   `json:"path"`
	Type           string   `json:"type"`
	AllowedOrigins []string `json:"allowed_origins"`
}

// New builds the manifest for a host executable at binPath — normally the
// cepm-owned launcher script (see internal/launcher), which gives Chrome one
// stable path across upgrades. The manifest format has no args field, so the
// launcher (or, legacy, the binary itself detecting the chrome-extension://
// argv) is responsible for entering native-host mode.
func New(binPath string) HostManifest {
	return HostManifest{
		Name:           helperext.HostName,
		Description:    "cepm native messaging host (updates and reloads managed Chrome extensions)",
		Path:           binPath,
		Type:           "stdio",
		AllowedOrigins: []string{"chrome-extension://" + helperext.ExtensionID() + "/"},
	}
}

// FileName is the required manifest file name: "<host name>.json".
func FileName() string { return helperext.HostName + ".json" }

// Install writes the manifest for one Chrome variant and returns the path.
func Install(variant, binPath string) (string, error) {
	dir, err := paths.NativeMessagingHostsDir(variant)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(New(binPath), "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, FileName())
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Read loads an installed manifest for doctor-style verification.
func Read(variant string) (*HostManifest, string, error) {
	dir, err := paths.NativeMessagingHostsDir(variant)
	if err != nil {
		return nil, "", err
	}
	path := filepath.Join(dir, FileName())
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, path, err
	}
	var m HostManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, path, fmt.Errorf("parse %s: %w", path, err)
	}
	return &m, path, nil
}

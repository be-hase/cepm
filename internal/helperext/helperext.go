// Package helperext embeds and installs the cepm helper extension — the small
// MV3 extension that performs reloads inside Chrome on behalf of cepm.
package helperext

import (
	"embed"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/be-hase/cepm/internal/extid"
)

//go:embed assets
var assets embed.FS

// Version is the helper extension version. Bump it whenever assets change;
// setup and the native host re-install the helper when it differs from the
// marker file.
const Version = "0.2.0"

// HostName is the native messaging host name shared by the helper extension,
// the host manifest, and the native host process.
const HostName = "com.github.be_hase.cepm"

// PublicKeyBase64 is the manifest "key": a base64 SPKI DER RSA public key.
// It pins the helper's extension ID to the same constant for every user
// (unpacked loads never verify signatures, so no private key is needed).
const PublicKeyBase64 = "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA0lLejiTvG5ElQmwA+FNOPTFTArbjNA65OVcj5zk3efV/myX/PK/TWO7oGT1BE/9zZfbozbaAMwrk6l8FoRVMGqmPaPCfdDdbtJ+ogS+6Evw9EJ3Tx+2oLUS+ddyzLbsMkoeXe0wvDIX4vOnwi1tULgTpxBlsSQ2zF5e8oZG+wMZRb3s8iPDwskfxrqFSgAaDuNH1vmZiRzOqnz+uLNwdjGHpMrP4KTeGbrAW71EBhYFT0eT47ScdgYodPS1LnfnIobpC5ALPIsIcJnDPKNfL//rlfi4/pGXRq08jOSb1z9nz4sMNTfiHl7shswdTSM1aUu9rsIF1fWmJPXVdQ2IbZQIDAQAB"

const versionMarker = ".cepm-helper-version"

// ExtensionID returns the fixed ID Chrome derives from PublicKeyBase64.
func ExtensionID() string {
	der, err := base64.StdEncoding.DecodeString(PublicKeyBase64)
	if err != nil {
		panic("helperext: invalid embedded public key: " + err.Error())
	}
	return extid.FromPublicKey(der)
}

// InstalledVersion reads the version marker of a previously installed helper,
// returning "" when none is installed.
func InstalledVersion(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, versionMarker))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Install writes the helper extension files into dir.
func Install(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmplData, err := assets.ReadFile("assets/manifest.json.tmpl")
	if err != nil {
		return err
	}
	tmpl, err := template.New("manifest").Parse(string(tmplData))
	if err != nil {
		return err
	}
	var manifest strings.Builder
	err = tmpl.Execute(&manifest, map[string]string{"Version": Version, "Key": PublicKeyBase64})
	if err != nil {
		return err
	}
	bg, err := assets.ReadFile("assets/background.js")
	if err != nil {
		return err
	}
	// Order matters: the version marker is what everything else uses to
	// decide whether the helper is up to date, so it must be written last
	// and only after the files it describes are in place. (A map would give
	// Go's randomized iteration order and could leave the marker claiming a
	// version whose files failed to write.)
	files := []struct {
		name    string
		content []byte
	}{
		{"manifest.json", []byte(manifest.String())},
		{"background.js", bg},
		{versionMarker, []byte(Version + "\n")},
	}
	for _, f := range files {
		if err := writeAtomic(filepath.Join(dir, f.name), f.content); err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
	}
	return nil
}

// writeAtomic replaces path in one step, so Chrome never observes a
// half-written manifest or worker script.
func writeAtomic(path string, content []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cepm-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

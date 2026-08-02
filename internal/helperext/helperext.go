// Package helperext embeds and installs the cepm helper extension — the small
// MV3 extension that performs reloads inside Chrome on behalf of cepm.
package helperext

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/be-hase/cepm/internal/extid"
)

//go:embed assets
var assets embed.FS

// Version is the helper extension version. Bump it whenever assets change;
// setup and the native host re-install the helper when it differs from the
// marker file.
const Version = "0.4.0"

// Protocol is the helper⇄host wire generation, embedded into the generated
// background.js from here so the running code announces what it actually
// is. The manifest version cannot serve: a refresh writes manifest.json
// first, so a failure on background.js leaves old code reporting the new
// version. Bump on any change to the messages exchanged with the host.
const Protocol = 1

// protocolPlaceholder is the line in the asset that carries Protocol into
// the generated file. Rendering fails loudly if it ever stops matching.
const protocolPlaceholder = "const HELPER_PROTOCOL = 0;"

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

// manifest is the helper's manifest.json shape. Built with json.Marshal —
// not a text template — so a future value with a quote or backslash cannot
// silently corrupt what Chrome loads (nmmanifest made the same choice).
type manifest struct {
	ManifestVersion int      `json:"manifest_version"`
	Name            string   `json:"name"`
	Version         string   `json:"version"`
	Description     string   `json:"description"`
	Key             string   `json:"key"`
	Permissions     []string `json:"permissions"`
	Background      struct {
		ServiceWorker string `json:"service_worker"`
	} `json:"background"`
}

// renderFiles produces the exact files this binary installs — the single
// source both Install and InstalledMatches work from, so "matches" can never
// drift from "would be written". Order matters: the version marker is what
// everything else uses to decide whether the helper is up to date, so it
// comes last and is written only after the files it describes are in place.
func renderFiles() ([]struct {
	name    string
	content []byte
}, error) {
	m := manifest{
		ManifestVersion: 3,
		Name:            "cepm helper",
		Version:         Version,
		Description:     "Reloads unpacked extensions managed by cepm. Do not remove.",
		Key:             PublicKeyBase64,
		// "storage" backs the crash-recovery marker background.js keeps
		// around its disable→enable pair.
		Permissions: []string{"management", "nativeMessaging", "alarms", "storage"},
	}
	m.Background.ServiceWorker = "background.js"
	manifestJSON, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	bg, err := assets.ReadFile("assets/background.js")
	if err != nil {
		return nil, err
	}
	rendered := strings.Replace(string(bg),
		protocolPlaceholder, fmt.Sprintf("const HELPER_PROTOCOL = %d;", Protocol), 1)
	if rendered == string(bg) {
		return nil, fmt.Errorf("background.js no longer contains %q", protocolPlaceholder)
	}
	bg = []byte(rendered)
	return []struct {
		name    string
		content []byte
	}{
		{"manifest.json", append(manifestJSON, '\n')},
		{"background.js", bg},
		{versionMarker, []byte(Version + "\n")},
	}, nil
}

// Install writes the helper extension files into dir.
func Install(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	files, err := renderFiles()
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := writeAtomic(filepath.Join(dir, f.name), f.content); err != nil {
			return fmt.Errorf("write %s: %w", f.name, err)
		}
	}
	return nil
}

// InstalledMatches reports whether every helper file at dir is byte-for-byte
// what this binary would install. The version marker alone is not evidence:
// it says nothing about a truncated or hand-edited file sitting next to it.
func InstalledMatches(dir string) bool {
	files, err := renderFiles()
	if err != nil {
		return false
	}
	for _, f := range files {
		got, err := os.ReadFile(filepath.Join(dir, f.name))
		if err != nil || !bytes.Equal(got, f.content) {
			return false
		}
	}
	return true
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

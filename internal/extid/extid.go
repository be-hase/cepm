// Package extid computes Chrome extension IDs.
//
// Chrome derives an extension ID from a SHA-256 hash: for unpacked extensions
// without a "key" in the manifest, the hash input is the absolute installation
// path; for extensions with a "key", it is the DER-encoded SPKI public key.
// The first 16 bytes of the hash are then encoded in "mpdecimal": each nibble
// 0x0-0xF maps to the letters 'a'-'p', yielding a 32-character ID.
package extid

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
)

// FromPath returns the ID Chrome assigns to an unpacked extension loaded from
// absPath. The path must be absolute and must match exactly what Chrome sees
// (no trailing slash, symlinks unresolved).
func FromPath(absPath string) (string, error) {
	if !filepath.IsAbs(absPath) {
		return "", fmt.Errorf("extension path must be absolute: %q", absPath)
	}
	return fromBytes([]byte(filepath.Clean(absPath))), nil
}

// FromPublicKey returns the ID of an extension whose manifest embeds the given
// DER-encoded SPKI public key ("key" field, base64-decoded).
func FromPublicKey(spkiDER []byte) string {
	return fromBytes(spkiDER)
}

func fromBytes(b []byte) string {
	sum := sha256.Sum256(b)
	id := make([]byte, 32)
	for i, c := range sum[:16] {
		id[i*2] = 'a' + c>>4
		id[i*2+1] = 'a' + c&0x0F
	}
	return string(id)
}

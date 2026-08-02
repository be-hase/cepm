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
	"encoding/base64"
	"fmt"
	"path/filepath"

	"github.com/be-hase/cepm/internal/paths"
)

// FromPath returns the ID Chrome assigns to an unpacked extension loaded from
// absPath.
//
// The path is canonicalized here rather than by callers. Chrome resolves
// symlinks before it hashes, so an unresolved path yields an id no extension
// will ever have — and nothing downstream can notice, because Validate
// re-derives ids the same way and agrees with itself. Leaving that to every call
// site is how it went wrong twice: the path a caller has is usually the one
// it needs for the filesystem, which is the unresolved one.
func FromPath(absPath string) (string, error) {
	if !filepath.IsAbs(absPath) {
		return "", fmt.Errorf("extension path must be absolute: %q", absPath)
	}
	canonical, err := paths.Canonical(absPath)
	if err != nil {
		return "", err
	}
	return fromBytes([]byte(canonical)), nil
}

// FromPublicKey returns the ID of an extension whose manifest embeds the given
// DER-encoded SPKI public key ("key" field, base64-decoded).
func FromPublicKey(spkiDER []byte) string {
	return fromBytes(spkiDER)
}

// ForExtension returns the ID Chrome will assign to the unpacked extension at
// absDir. Every caller must go through this: an extension that pins its ID by
// embedding a "key" in its manifest (common when the ID has to be allowlisted
// elsewhere) gets its ID from that key, not from its path, and computing the
// wrong one makes every later lookup — reload, list, doctor, cleanup —
// silently miss.
func ForExtension(absDir, manifestKey string) (string, error) {
	if manifestKey == "" {
		return FromPath(absDir)
	}
	der, err := base64.StdEncoding.DecodeString(manifestKey)
	if err != nil {
		return "", fmt.Errorf("manifest.json of %s has an invalid \"key\": %w", absDir, err)
	}
	return FromPublicKey(der), nil
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

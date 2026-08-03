//go:build !unix

package cli

import "io/fs"

// deviceOf has no portable answer off unix; callers treat "unknown" as "the
// same filesystem" and let a rename fail loudly if it is not.
func deviceOf(fs.FileInfo) (uint64, bool) { return 0, false }

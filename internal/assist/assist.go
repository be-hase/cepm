// Package assist provides small OS-dependent conveniences used by the CLI to
// smooth over Chrome's one manual step (Load unpacked): putting the target
// path on the clipboard and opening chrome://extensions. Everything here is
// best-effort — failures must never break the calling command.
package assist

import (
	"os"
)

// IsTTY reports whether the CLI is talking to an interactive terminal (both
// directions), which gates prompts and the load-confirmation ceremony. It is
// a variable so tests can exercise the interactive paths.
var IsTTY = func() bool {
	return isCharDevice(os.Stdin) && isCharDevice(os.Stdout)
}

func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

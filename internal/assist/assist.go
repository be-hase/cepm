// Package assist provides small OS-dependent conveniences used by the CLI to
// smooth over Chrome's one manual step (Load unpacked): putting the target
// path on the clipboard and opening chrome://extensions. Everything here is
// best-effort — failures must never break the calling command.
package assist

import (
	"os"
)

// IsTTY reports whether the CLI is talking to an interactive terminal (both
// directions), which gates prompts and the load-confirmation ceremony.
//
// This and the two below are variables because the interactive paths have to
// be reachable from tests — and because these are the functions that reach
// out of the process and touch the developer's own browser and clipboard,
// which a test run must never do.
var IsTTY = func() bool {
	return isCharDevice(os.Stdin) && isCharDevice(os.Stdout)
}

// CopyToClipboard puts s on the system clipboard.
var CopyToClipboard = copyToClipboard

// OpenExtensionsPage opens chrome://extensions in the user's Chrome.
var OpenExtensionsPage = openExtensionsPage

// chromeVariant selects which Chrome OpenExtensionsPage targets; "stable"
// until SetChromeVariant says otherwise.
var chromeVariant = "stable"

// SetChromeVariant records which Chrome variant the user set up, so the
// ceremony opens the browser that actually has the helper registered rather
// than always stock Chrome.
func SetChromeVariant(v string) {
	if v != "" {
		chromeVariant = v
	}
}

func isCharDevice(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

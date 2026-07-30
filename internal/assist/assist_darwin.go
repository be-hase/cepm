//go:build darwin

package assist

import (
	"os/exec"
	"strings"
)

// CopyToClipboard puts s on the system clipboard.
func CopyToClipboard(s string) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run()
}

// OpenExtensionsPage opens chrome://extensions in Chrome. chrome:// URLs
// cannot be opened through the default URL handler, so target Chrome
// explicitly.
func OpenExtensionsPage() error {
	return exec.Command("open", "-a", "Google Chrome", "chrome://extensions/").Run()
}

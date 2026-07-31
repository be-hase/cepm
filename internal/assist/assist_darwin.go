//go:build darwin

package assist

import (
	"os/exec"
	"strings"
)

// copyToClipboard puts s on the system clipboard.
func copyToClipboard(s string) error {
	cmd := exec.Command("pbcopy")
	cmd.Stdin = strings.NewReader(s)
	return cmd.Run()
}

// openExtensionsPage opens chrome://extensions in Chrome. chrome:// URLs
// cannot be opened through the default URL handler, so target Chrome
// explicitly.
func openExtensionsPage() error {
	return exec.Command("open", "-a", "Google Chrome", "chrome://extensions/").Run()
}

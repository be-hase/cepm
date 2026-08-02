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

// chromeApps maps a --chrome-variant value to the macOS application name.
var chromeApps = map[string]string{
	"stable":   "Google Chrome",
	"beta":     "Google Chrome Beta",
	"dev":      "Google Chrome Dev",
	"canary":   "Google Chrome Canary",
	"chromium": "Chromium",
}

// openExtensionsPage opens chrome://extensions in the configured Chrome.
// chrome:// URLs cannot be opened through the default URL handler, so the
// application is targeted explicitly.
func openExtensionsPage() error {
	app := chromeApps[chromeVariant]
	if app == "" {
		app = "Google Chrome"
	}
	return exec.Command("open", "-a", app, "chrome://extensions/").Run()
}

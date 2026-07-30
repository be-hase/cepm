//go:build darwin

package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// ChromeVariants lists the supported --chrome-variant values.
var ChromeVariants = []string{"stable", "beta", "dev", "canary", "chromium"}

var chromeVariantDirs = map[string]string{
	"stable":   "Google/Chrome",
	"beta":     "Google/Chrome Beta",
	"dev":      "Google/Chrome Dev",
	"canary":   "Google/Chrome Canary",
	"chromium": "Chromium",
}

// NativeMessagingHostsDir returns the directory where Chrome looks for
// user-level native messaging host manifests.
func NativeMessagingHostsDir(variant string) (string, error) {
	sub, ok := chromeVariantDirs[variant]
	if !ok {
		return "", fmt.Errorf("unknown chrome variant %q (supported: %v)", variant, ChromeVariants)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, "Library", "Application Support", sub, "NativeMessagingHosts"), nil
}

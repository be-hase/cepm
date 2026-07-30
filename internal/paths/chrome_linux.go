//go:build linux

package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// ChromeVariants lists the supported --chrome-variant values.
// (Chrome Canary does not exist on Linux.)
var ChromeVariants = []string{"stable", "beta", "dev", "chromium"}

var chromeVariantDirs = map[string]string{
	"stable":   "google-chrome",
	"beta":     "google-chrome-beta",
	"dev":      "google-chrome-unstable",
	"chromium": "chromium",
}

// NativeMessagingHostsDir returns the directory where Chrome looks for
// user-level native messaging host manifests
// ($XDG_CONFIG_HOME/google-chrome/NativeMessagingHosts by default).
func NativeMessagingHostsDir(variant string) (string, error) {
	sub, ok := chromeVariantDirs[variant]
	if !ok {
		return "", fmt.Errorf("unknown chrome variant %q (supported: %v)", variant, ChromeVariants)
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve config directory: %w", err)
	}
	return filepath.Join(cfg, sub, "NativeMessagingHosts"), nil
}

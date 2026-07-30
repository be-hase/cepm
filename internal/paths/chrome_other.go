//go:build !darwin

package paths

import "fmt"

// ChromeVariants lists the supported --chrome-variant values.
var ChromeVariants = []string{"stable"}

// NativeMessagingHostsDir is not implemented outside macOS yet.
// TODO: Linux (~/.config/google-chrome/NativeMessagingHosts) and
// Windows (registry HKCU\Software\Google\Chrome\NativeMessagingHosts).
func NativeMessagingHostsDir(variant string) (string, error) {
	return "", fmt.Errorf("cepm currently supports macOS only (variant %q)", variant)
}

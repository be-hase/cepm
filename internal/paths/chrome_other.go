//go:build !darwin && !linux

package paths

import "fmt"

// ChromeVariants lists the supported --chrome-variant values.
var ChromeVariants = []string{"stable"}

// NativeMessagingHostsDir is not implemented on this platform yet.
// TODO: Windows needs a registry entry
// (HKCU\Software\Google\Chrome\NativeMessagingHosts) instead of a well-known
// directory, plus a .bat launcher and a UTF-16-based extension ID scheme.
func NativeMessagingHostsDir(variant string) (string, error) {
	return "", fmt.Errorf("cepm currently supports macOS and Linux only (variant %q)", variant)
}

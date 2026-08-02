//go:build linux

package assist

import (
	"errors"
	"os/exec"
	"strings"
)

// copyToClipboard puts s on the clipboard, trying Wayland then X11 helpers.
func copyToClipboard(s string) error {
	for _, c := range [][]string{
		{"wl-copy"},
		{"xclip", "-selection", "clipboard"},
		{"xsel", "--clipboard", "--input"},
	} {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Stdin = strings.NewReader(s)
		return cmd.Run()
	}
	return errors.New("no clipboard helper found (install wl-clipboard or xclip)")
}

// chromeBins maps a --chrome-variant value to the binaries it usually
// installs, tried first; any Chrome is a last resort (a wrong window still
// beats sending the user to type the URL).
var chromeBins = map[string][]string{
	"stable":   {"google-chrome", "google-chrome-stable"},
	"beta":     {"google-chrome-beta"},
	"dev":      {"google-chrome-unstable"},
	"chromium": {"chromium", "chromium-browser"},
}

// openExtensionsPage opens chrome://extensions in the configured Chrome.
// xdg-open cannot handle chrome:// URLs, so a Chrome binary is invoked
// directly.
func openExtensionsPage() error {
	candidates := append([]string{}, chromeBins[chromeVariant]...)
	candidates = append(candidates, "google-chrome", "google-chrome-stable", "chromium", "chromium-browser")
	for _, bin := range candidates {
		if _, err := exec.LookPath(bin); err == nil {
			return exec.Command(bin, "chrome://extensions/").Start()
		}
	}
	return errors.New("no Chrome binary found on PATH")
}

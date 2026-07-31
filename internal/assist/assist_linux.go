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

// openExtensionsPage opens chrome://extensions in Chrome. xdg-open cannot
// handle chrome:// URLs, so invoke a Chrome binary directly.
func openExtensionsPage() error {
	for _, bin := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
		if _, err := exec.LookPath(bin); err == nil {
			return exec.Command(bin, "chrome://extensions/").Start()
		}
	}
	return errors.New("no Chrome binary found on PATH")
}

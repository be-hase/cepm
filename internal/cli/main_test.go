package cli

import (
	"os"
	"testing"

	"github.com/be-hase/cepm/internal/assist"
)

// TestMain cuts the package off from the developer's own machine. The load
// ceremony opens chrome://extensions and writes the clipboard, and tests that
// force the interactive path used to do both for real: every run added a tab
// to the running Chrome and replaced whatever the developer had copied.
//
// Stubbing here rather than per test means a future test cannot reintroduce
// it by forgetting.
func TestMain(m *testing.M) {
	assist.CopyToClipboard = func(string) error { return nil }
	assist.OpenExtensionsPage = func() error { return nil }
	os.Exit(m.Run())
}

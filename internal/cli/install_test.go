package cli

import (
	"os"
	"strings"
	"testing"
)

// A clone without a state entry (an install killed between the rename and
// the save) must not be met with "run cepm uninstall": uninstall does not
// know the name, and the old advice was a dead end.
func TestInstallNamesTheRecoveryForAnUnregisteredClone(t *testing.T) {
	startFakeHost(t)
	dir, err := updaterRepoDir("tools")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err = run(t, "", "install", "https://example.com/t/tools.git", "--name", "tools")
	if err == nil {
		t.Fatal("install should refuse the occupied directory")
	}
	if strings.Contains(err.Error(), "cepm uninstall") {
		t.Errorf("must not advise uninstall for an unregistered clone: %v", err)
	}
	if !strings.Contains(err.Error(), "interrupted install") {
		t.Errorf("should explain what this probably is: %v", err)
	}
}

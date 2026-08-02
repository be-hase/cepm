package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/nmmanifest"
	"github.com/be-hase/cepm/internal/paths"
)

// A registration cepm could not remove leaves two Chromes able to start a
// host, of which only the one that wins the leader lock gets the reloads.
// Printing that as a warning and exiting 0 would let a scripted setup — and
// a person skimming the output — treat the switch as done.
func TestSetupFailsWhenAnotherChromeStaysRegistered(t *testing.T) {
	if len(paths.ChromeVariants) < 2 {
		t.Skip("only one Chrome variant on this platform")
	}
	dir, err := os.MkdirTemp("/tmp", "cepm-cli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("CEPM_HOME", dir)
	t.Setenv("HOME", dir) // the variant manifest directories hang off this

	keep, other := paths.ChromeVariants[0], paths.ChromeVariants[1]
	otherPath, err := nmmanifest.Install(other, filepath.Join(dir, "bin", "cepm-host"))
	if err != nil {
		t.Fatal(err)
	}
	otherDir := filepath.Dir(otherPath)
	if err := os.Chmod(otherDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(otherDir, 0o700) })

	out, err := run(t, "", "setup", "--chrome-variant", keep)
	if err == nil {
		t.Fatalf("setup must not report success while another Chrome is still registered:\n%s", out)
	}
	if !strings.Contains(err.Error(), otherPath) {
		t.Errorf("the error must name the file to delete, got: %v", err)
	}
}

// The uninstall wait must reach the host without a deadline of its own.
// A deadline here defeats everything downstream: the host waits on the
// helper's heartbeat precisely because nothing closes Chrome's dialog when
// a clock runs out, and this call is made while holding the update lock so
// the id cannot be registered again under a live "Remove" button.
//
// Pinned by reading the source, because the alternative is a test that
// waits out a real deadline — and the deadline that mattered was three
// minutes. Go cannot see a context's absence of a deadline from the far
// side of a socket.
func TestUninstallIsNotGivenADeadlineOfItsOwn(t *testing.T) {
	src, err := os.ReadFile("disable.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func uninstallViaChrome(")
	if start < 0 {
		t.Fatal("uninstallViaChrome is gone; this test needs rewriting")
	}
	end := strings.Index(body[start:], "\n}\n")
	if end < 0 {
		t.Fatal("could not find the end of uninstallViaChrome")
	}
	fn := body[start : start+end]
	if !strings.Contains(fn, "ipc.Uninstall(ctx, id)") {
		t.Fatal("uninstallViaChrome no longer calls ipc.Uninstall directly; this test needs rewriting")
	}
	if strings.Contains(fn, "WithTimeout") || strings.Contains(fn, "WithDeadline") {
		t.Error("a deadline here releases the update lock with Chrome's dialog still on screen")
	}
}

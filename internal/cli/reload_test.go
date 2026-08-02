package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/paths"
	"github.com/be-hase/cepm/internal/state"
)

// A bare "cepm reload" that reloads nothing has failed, and a script must be
// able to see that in the exit code — unlike update, whose pull already
// succeeded and applies on the next launch. Each failure mode must be
// non-zero; success must stay zero.
func TestReloadExitCodes(t *testing.T) {
	t.Run("host down", func(t *testing.T) {
		dir, err := os.MkdirTemp("/tmp", "cepm-cli")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		t.Setenv("CEPM_HOME", dir)
		if err := paths.EnsureLayout(); err != nil {
			t.Fatal(err)
		}
		seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
		if _, err := run(t, "", "reload", "tools"); err == nil {
			t.Error("reload with no host must exit non-zero")
		}
	})

	t.Run("host error", func(t *testing.T) {
		h := startFakeHost(t, idA)
		h.reloadHostError = "helper not connected"
		seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
		if _, err := run(t, "", "reload", "tools"); err == nil {
			t.Error("a host-level reload error must exit non-zero")
		}
	})

	t.Run("per-id error", func(t *testing.T) {
		h := startFakeHost(t, idA)
		h.failReloads = true
		seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
		out, err := run(t, "", "reload", "tools")
		if err == nil {
			t.Errorf("a per-id reload failure must exit non-zero:\n%s", out)
		}
	})

	t.Run("missing answers", func(t *testing.T) {
		h := startFakeHost(t, idA)
		h.dropReloadResults = true
		seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
		if _, err := run(t, "", "reload", "tools"); err == nil {
			t.Error("an unanswered reload must exit non-zero")
		}
	})

	t.Run("not installed", func(t *testing.T) {
		h := startFakeHost(t, idA)
		h.reloadNotInstalled = true
		seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
		if _, err := run(t, "", "reload", "tools"); err == nil {
			t.Error("an extension not loaded in Chrome was not reloaded; reload must exit non-zero")
		}
	})

	t.Run("skipped in Chrome", func(t *testing.T) {
		h := startFakeHost(t, idA)
		h.reloadStatusByID = map[string]string{idA: ipc.StatusSkippedDisabled}
		seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
		if _, err := run(t, "", "reload", "tools"); err == nil {
			t.Error("a skipped extension was not reloaded; reload must exit non-zero")
		}
	})

	t.Run("mixed reloaded and skipped", func(t *testing.T) {
		h := startFakeHost(t, idA, idB)
		h.reloadStatusByID = map[string]string{idB: ipc.StatusSkippedNotUnpacked}
		seedRepo(t, "tools",
			state.Extension{Dir: "a", Name: "A", ID: idA, Key: keyA},
			state.Extension{Dir: "b", Name: "B", ID: idB, Key: keyB},
		)
		out, err := run(t, "", "reload", "tools")
		if err == nil {
			t.Errorf("one of two requested reloads did not happen; reload must exit non-zero:\n%s", out)
		}
	})

	t.Run("success", func(t *testing.T) {
		startFakeHost(t, idA)
		seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
		if out, err := run(t, "", "reload", "tools"); err != nil {
			t.Errorf("a successful reload must exit zero: %v\n%s", err, out)
		}
	})
}

// An extension that vanished from Chrome between enable's list and its
// reload still needs the one-time Load unpacked: the not_installed answer
// must route it back into the load ceremony instead of being shrugged off.
func TestEnableRoutesVanishedExtensionBackToTheCeremony(t *testing.T) {
	h := startFakeHost(t, idA)
	h.reloadNotInstalled = true // listed as loaded, but gone by reload time
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA, Disabled: true})

	out, err := run(t, "", "enable", "tools/ext")
	if err != nil {
		t.Fatalf("enable failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Load unpacked") {
		t.Errorf("the vanished extension should re-enter the load ceremony:\n%s", out)
	}
}

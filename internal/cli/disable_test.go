package cli

import (
	"testing"

	"github.com/be-hase/cepm/internal/state"
)

// The "remove it from Chrome too?" answer is collected while nothing is
// locked, so it can be stale by the time it is acted on: another cepm can
// change the extension's standing while the prompt is open. Acting on the
// old answer would remove an extension whose registration just changed —
// disable must notice and abort with nothing touched.
func TestDisableAbortsWhenStateChangesDuringThePrompt(t *testing.T) {
	h := startFakeHost(t, idA)
	interactive(t)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})

	// The list call happens while the prompt is being prepared: the window
	// in which a cepm in another terminal can complete.
	h.onList = func() {
		h.onList = nil
		st, err := state.Load()
		if err != nil {
			t.Error(err)
			return
		}
		st.Repos["tools"].Extensions[0].Disabled = true
		if err := st.Save(); err != nil {
			t.Error(err)
		}
	}

	_, err := run(t, "y\n", "disable", "tools/ext")
	if err == nil {
		t.Fatal("disable must abort when the state changed during the prompt")
	}
	if len(h.uninstalled) != 0 {
		t.Errorf("nothing may be removed from Chrome after an abort, removed: %v", h.uninstalled)
	}
}

// The happy path still works end to end: an approved removal reaches Chrome,
// and it does so via the host's authorization (the fake host enforces it).
func TestDisableRemovesFromChromeWhenApproved(t *testing.T) {
	h := startFakeHost(t, idA)
	interactive(t)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})

	out, err := run(t, "y\n", "disable", "tools/ext")
	if err != nil {
		t.Fatalf("disable failed: %v\n%s", err, out)
	}
	if len(h.uninstalled) != 1 || h.uninstalled[0] != idA {
		t.Errorf("expected the approved extension to be removed from Chrome, removed: %v", h.uninstalled)
	}
	st, err := state.Load()
	if err != nil {
		t.Fatal(err)
	}
	if e := st.Repos["tools"].FindExtension("ext"); e == nil || !e.Disabled {
		t.Errorf("the extension should be recorded as disabled, got %+v", e)
	}
}

// Enabling an extension Chrome still has loaded reloads it on the spot:
// code pulled while it sat disabled is already on disk, and waiting for the
// next update tick would leave stale code running.
func TestEnableReloadsAnAlreadyLoadedExtension(t *testing.T) {
	h := startFakeHost(t, idA)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA, Disabled: true})
	if out, err := run(t, "", "enable", "tools/ext"); err != nil {
		t.Fatalf("enable failed: %v\n%s", err, out)
	}
	if len(h.reloaded) != 1 || h.reloaded[0] != idA {
		t.Errorf("expected the loaded extension to be reloaded, got %v", h.reloaded)
	}
}

// "cepm reload tools tools" must reach Chrome once per extension: duplicate
// ids would run two interleaved disable/enable cycles for one extension.
func TestReloadDeduplicatesRepeatedRepos(t *testing.T) {
	h := startFakeHost(t, idA)
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})
	if out, err := run(t, "", "reload", "tools", "tools"); err != nil {
		t.Fatalf("reload failed: %v\n%s", err, out)
	}
	if len(h.reloaded) != 1 || h.reloaded[0] != idA {
		t.Errorf("expected exactly one reload of %s, got %v", idA, h.reloaded)
	}
}

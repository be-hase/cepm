package cli

import (
	"strings"
	"testing"

	"github.com/be-hase/cepm/internal/state"
)

// An extension enabled in cepm but switched off in chrome://extensions is
// the one state where pulls happen but reloads never do (updates respect the
// Chrome-side switch). Without a diagnostic, "updates don't work" doctor
// output reads all green.
func TestDoctorFlagsExtensionDisabledInChrome(t *testing.T) {
	h := startFakeHost(t, idA)
	h.disabledInChrome = map[string]bool{idA: true}
	seedRepo(t, "tools", state.Extension{Dir: "ext", Name: "Ext", ID: idA, Key: keyA})

	out, _ := run(t, "", "doctor")
	if !strings.Contains(out, "turned off in chrome://extensions") {
		t.Errorf("doctor should flag the Chrome-side disable:\n%s", out)
	}

	// And not when Chrome agrees the extension is on.
	h.mu.Lock()
	h.disabledInChrome = nil
	h.mu.Unlock()
	out, _ = run(t, "", "doctor")
	if strings.Contains(out, "turned off in chrome://extensions") {
		t.Errorf("doctor must not flag an extension Chrome has enabled:\n%s", out)
	}
}

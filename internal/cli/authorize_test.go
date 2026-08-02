package cli

import (
	"fmt"
	"testing"

	"github.com/be-hase/cepm/internal/nmhost"
)

// The two relay sets are not the same set, and the difference is what keeps
// a stale id list from reaching the wrong operation: an orphan record is
// something cleanup may remove from Chrome, never something to reload. (The
// state file itself is trusted — see internal/state — so this bounds
// mistakes, not a hostile local user.)
func TestReloadAndRemovalAuthorizeDifferentSets(t *testing.T) {
	startFakeHost(t)
	writeRawState(t, fmt.Sprintf(`{"version":6,"repos":{},
      "orphans":[{"id":%q,"name":"X","reason":"uninstalled"}]}`, idA))

	if _, err := nmhost.AuthorizeRemoval([]string{idA}); err != nil {
		t.Errorf("an orphan record must be removable: %v", err)
	}
	if _, err := nmhost.AuthorizeReload([]string{idA}); err == nil {
		t.Error("an orphan is not a live extension; reload must be refused")
	}
}

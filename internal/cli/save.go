package cli

import (
	"fmt"
	"io"

	"github.com/be-hase/cepm/internal/state"
)

// saveState writes the state and turns a durability-only failure into a
// warning.
//
// The distinction is not cosmetic. state.Save replaces state.json before it
// flushes the directory, so a failure in that last step leaves the new state
// live: every reader already sees it. A caller that stopped there would go
// on to skip the work the state now describes — the Chrome load ceremony
// after an enable, the reloads an update owes, the clone an install just
// registered — and the next run, reading the state that did land, would
// consider all of it already done. The world would then be permanently
// behind the state, which is the inversion the atomic write exists to
// prevent, traded for a risk only a crash in the next moments could realise.
//
// Not for callers that destroy something next. Deleting a clone, or moving
// one to a backup, cannot be undone by the state reverting: a crash before
// the flush lands brings the old state back with its files already gone.
// Those callers — uninstall, reset — take st.Save() directly and stop.
func saveState(out io.Writer, st *state.State) error {
	err := st.Save()
	if err != nil && state.Committed(err) {
		fmt.Fprintf(out, "Warning: %v\n", err)
		return nil
	}
	return err
}

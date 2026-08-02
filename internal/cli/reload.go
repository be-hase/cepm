package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/state"
)

func newReloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reload [name...]",
		Short: "Reload registered extensions in Chrome without pulling",
		Long: `Reload reloads extensions in Chrome without touching git. Useful after
editing files locally. With no arguments every registered extension is
reloaded; otherwise pass repository names.`,
		GroupID: "ext",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.Load()
			if err != nil {
				return err
			}
			targets := args
			if len(targets) == 0 {
				targets = st.RepoNames()
			}
			var items []reloadItem
			skipped := 0
			// "cepm reload tools tools" must not reload twice: the helper
			// would run two interleaved disable/enable cycles for one id.
			seen := map[string]bool{}
			for _, name := range targets {
				r, ok := st.Repos[name]
				if !ok {
					return fmt.Errorf("repository %q is not registered (see cepm list)", name)
				}
				for _, e := range r.Extensions {
					if seen[e.ID] {
						continue
					}
					seen[e.ID] = true
					if !e.Enabled() {
						skipped++
						continue
					}
					items = append(items, reloadItem{ID: e.ID, Name: e.Name})
				}
			}
			if len(items) == 0 {
				return fmt.Errorf("no enabled extensions to reload (see cepm list)")
			}
			if skipped > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "(%d available extension(s) skipped)\n", skipped)
			}
			// Unlike update — where the pull already succeeded and applies
			// on the next launch — a reload that reloads nothing has simply
			// failed, and a script must be able to see that.
			outcome := reloadExtensions(cmd.Context(), cmd.OutOrStdout(), items)
			// Unlike update, this command has no diff to consult, so it
			// cannot name the extensions whose manifest changed. Saying
			// nothing would let "↻ reloaded" stand for more than happened:
			// a reload never applies manifest.json.
			if outcome.Reloaded > 0 {
				fmt.Fprintf(cmd.OutOrStdout(),
					"\nNote: a reload re-reads code, not manifest.json — Chrome keeps the manifest it\n"+
						"cached. If you changed permissions or the version, restart Chrome (or click\n"+
						"Reload in chrome://extensions).\n")
			}
			if outcome.HostUnreachable {
				return errors.New("nothing was reloaded: the cepm host is not reachable (is Chrome running with the helper loaded?)")
			}
			// Anything short of an actual reload is a miss for this command:
			// "not loaded in Chrome" or "turned off in Chrome" may be fine
			// for update, but the user asked for these reloads by name.
			if outcome.Reloaded < len(items) {
				return fmt.Errorf("%d of %d requested reload(s) did not happen (%d failed, %d skipped, %d not loaded in Chrome — see above)",
					len(items)-outcome.Reloaded, len(items), outcome.Failed, outcome.Skipped, len(outcome.NotInstalledIDs))
			}
			return nil
		},
	}
}

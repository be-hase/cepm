package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/assist"
	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/updater"
)

func newCleanupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cleanup",
		Short: "Remove broken Chrome entries left by renamed or deleted extensions",
		Long: `When a repo renames or deletes an extension directory, the copy loaded in
Chrome is left pointing at a path that no longer exists. Cleanup removes
those entries via Chrome's own confirmation dialog.`,
		GroupID: "ext",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			// Removing an extension requires the user to answer a dialog in
			// Chrome, so this cannot run unattended.
			if !assist.IsTTY() {
				return errors.New("cepm cleanup needs a terminal: Chrome asks for confirmation before removing an extension")
			}

			// Which entries to consider. Collected without the lock: each one
			// is re-checked under it before anything happens to it.
			st, err := state.Load()
			if err != nil {
				return err
			}
			type staleRef struct {
				repo string
				s    state.StaleExtension
			}
			var records []staleRef
			for _, name := range st.RepoNames() {
				for _, s := range st.Repos[name].Stale {
					records = append(records, staleRef{name, s})
				}
			}
			// Entries whose repository was uninstalled before they were
			// removed from Chrome.
			for _, o := range st.Orphans {
				records = append(records, staleRef{"(uninstalled repo)", o})
			}
			if len(records) == 0 {
				fmt.Fprintln(out, "Nothing to clean up.")
				return nil
			}

			// One id at a time, each holding the update lock only for its own
			// dialog. Holding it across the whole batch would scale with the
			// number of entries — three unanswered dialogs already exceed the
			// five minutes an update waits, and a background update that
			// fails does not retry until its next interval.
			var removedIDs, goneIDs, leftIDs []string
			handled := map[string]bool{}
			for _, p := range records {
				if handled[p.s.ID] {
					continue // the same id can be recorded more than once
				}
				handled[p.s.ID] = true

				err := updater.WithLock(cmd.Context(), func() error {
					// Re-read inside the lock: an update between entries may
					// have registered this extension again, and removing a
					// live extension is the one thing cleanup must never do.
					st, err := state.Load()
					if err != nil {
						return err
					}
					if st.IsLive(p.s.ID) {
						fmt.Fprintf(out, "%s: %q is registered again; leaving it alone\n", p.repo, p.s.Name)
						return nil
					}
					listCtx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
					loaded, err := ipc.ListChrome(listCtx)
					cancel()
					if errors.Is(err, ipc.ErrHostNotRunning) {
						return errors.New("Chrome is not reachable; start Chrome (with the helper loaded) and retry")
					}
					if err != nil {
						return err
					}
					if !containsID(loaded, p.s.ID) {
						goneIDs = append(goneIDs, p.s.ID) // already gone from Chrome
					} else {
						fmt.Fprintf(out, "%s: %q (%s)\n", p.repo, p.s.Name, p.s.Reason)
						// The dialog wait stays inside the lock: this is the
						// window where a revival would be fatal.
						uninstallViaChrome(cmd.Context(), cmd, p.s.ID, p.s.Name)
						stillCtx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
						still, listErr := ipc.ListChrome(stillCtx)
						cancel()
						if listErr == nil && !containsID(still, p.s.ID) {
							removedIDs = append(removedIDs, p.s.ID)
						} else {
							leftIDs = append(leftIDs, p.s.ID)
							return nil // nothing to clear from state
						}
					}
					for _, r := range st.Repos {
						r.RemoveStale(p.s.ID)
					}
					st.RemoveOrphan(p.s.ID)
					return st.Save()
				})
				if err != nil {
					return err
				}
			}
			if len(removedIDs) > 0 {
				fmt.Fprintf(out, "✔ Removed %d extension(s) from Chrome.\n", len(removedIDs))
			}
			if len(goneIDs) > 0 {
				fmt.Fprintf(out, "✔ Dropped %d record(s) for entries no longer in Chrome.\n", len(goneIDs))
			}
			if len(leftIDs) > 0 {
				fmt.Fprintf(out, "%d entr%s left in Chrome (cancelled or failed); run cepm cleanup again to retry.\n",
					len(leftIDs), pluralY(len(leftIDs)))
			}
			return nil
		},
	}
}

func containsID(exts []ipc.ChromeExt, id string) bool {
	for _, e := range exts {
		if e.ID == id {
			return true
		}
	}
	return false
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

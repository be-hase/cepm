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

			// Everything — including the dialog waits — runs under the update
			// lock. An automatic update pausing for a few minutes is cheap;
			// the alternative is a window in which an update revives an
			// extension after we checked it and before the user confirms the
			// dialog, and cleanup uninstalls something that works again.
			// (Dialog waits are bounded by the host's uninstall timeout, so
			// the lock cannot be held indefinitely.)
			var removedIDs, goneIDs, leftIDs []string
			err := updater.WithLock(cmd.Context(), func() error {
				listCtx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
				loaded, err := ipc.ListChrome(listCtx)
				cancel()
				if errors.Is(err, ipc.ErrHostNotRunning) {
					return errors.New("Chrome is not reachable; start Chrome (with the helper loaded) and retry")
				}
				if err != nil {
					return err
				}
				loadedSet := map[string]bool{}
				for _, e := range loaded {
					loadedSet[e.ID] = true
				}

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

				// One decision per id: the same id can be recorded by more
				// than one repo, and counting records would misreport what
				// happened in Chrome.
				handled := map[string]bool{}
				for _, p := range records {
					if handled[p.s.ID] {
						continue
					}
					handled[p.s.ID] = true
					switch {
					case st.IsLive(p.s.ID):
						// Registered again (a reinstall, a reverted deletion):
						// no longer ours to remove. The record is dropped by
						// Save's normalization below.
						fmt.Fprintf(out, "%s: %q is registered again; leaving it alone\n", p.repo, p.s.Name)
					case !loadedSet[p.s.ID]:
						goneIDs = append(goneIDs, p.s.ID) // already gone from Chrome
					default:
						fmt.Fprintf(out, "%s: %q (%s)\n", p.repo, p.s.Name, p.s.Reason)
						uninstallViaChrome(cmd.Context(), cmd, p.s.ID, p.s.Name)
						stillCtx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
						still, listErr := ipc.ListChrome(stillCtx)
						cancel()
						if listErr == nil && !containsID(still, p.s.ID) {
							removedIDs = append(removedIDs, p.s.ID)
						} else {
							leftIDs = append(leftIDs, p.s.ID)
						}
					}
				}

				for _, id := range append(append([]string(nil), goneIDs...), removedIDs...) {
					for _, r := range st.Repos {
						r.RemoveStale(id)
					}
					st.RemoveOrphan(id)
				}
				return st.Save()
			})
			if err != nil {
				return err
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

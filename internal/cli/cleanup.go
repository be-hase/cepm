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

			// Removing an extension requires the user to answer a dialog in
			// Chrome, so this cannot run unattended.
			if !assist.IsTTY() {
				return errors.New("cepm cleanup needs a terminal: Chrome asks for confirmation before removing an extension")
			}

			// Collect first, act second: the removals below wait on the user,
			// and holding the update lock through that would stall the
			// periodic updater for minutes.
			type staleRef struct {
				repo string
				s    state.StaleExtension
			}
			var pending []staleRef
			var goneIDs []string
			st, err := state.Load()
			if err != nil {
				return err
			}
			for _, name := range st.RepoNames() {
				for _, s := range st.Repos[name].Stale {
					if loadedSet[s.ID] {
						pending = append(pending, staleRef{name, s})
					} else {
						goneIDs = append(goneIDs, s.ID) // already gone from Chrome
					}
				}
			}
			if len(pending) == 0 && len(goneIDs) == 0 {
				fmt.Fprintln(out, "Nothing to clean up.")
				return nil
			}

			removed := map[string]bool{}
			for _, p := range pending {
				fmt.Fprintf(out, "%s: %q (%s)\n", p.repo, p.s.Name, p.s.Reason)
				uninstallViaChrome(cmd.Context(), cmd, p.s.ID, p.s.Name)
				stillCtx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
				still, listErr := ipc.ListChrome(stillCtx)
				cancel()
				if listErr == nil && !containsID(still, p.s.ID) {
					removed[p.s.ID] = true
				}
			}

			err = updater.WithLock(cmd.Context(), func() error {
				st, err := state.Load()
				if err != nil {
					return err
				}
				for _, r := range st.Repos {
					for _, id := range goneIDs {
						r.RemoveStale(id)
					}
					for id := range removed {
						r.RemoveStale(id)
					}
				}
				return st.Save()
			})
			if err != nil {
				return err
			}
			if len(removed) > 0 {
				fmt.Fprintf(out, "✔ Removed %d extension(s) from Chrome.\n", len(removed))
			}
			if len(goneIDs) > 0 {
				fmt.Fprintf(out, "✔ Dropped %d record(s) for entries no longer in Chrome.\n", len(goneIDs))
			}
			if skipped := len(pending) - len(removed); skipped > 0 {
				fmt.Fprintf(out, "%d entr%s left in Chrome; run cepm cleanup again to retry.\n", skipped, pluralY(skipped))
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

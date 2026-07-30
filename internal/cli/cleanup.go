package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

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

			cleaned := 0
			err = updater.WithLock(cmd.Context(), func() error {
				st, err := state.Load()
				if err != nil {
					return err
				}
				for _, name := range st.RepoNames() {
					r := st.Repos[name]
					for _, s := range append([]state.StaleExtension(nil), r.Stale...) {
						if !loadedSet[s.ID] {
							r.RemoveStale(s.ID) // already gone from Chrome
							cleaned++
							continue
						}
						fmt.Fprintf(out, "%s: %q (%s)\n", name, s.Name, s.Reason)
						uninstallViaChrome(cmd.Context(), cmd, s.ID, s.Name)
						stillCtx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
						still, listErr := ipc.ListChrome(stillCtx)
						cancel()
						if listErr == nil && !containsID(still, s.ID) {
							r.RemoveStale(s.ID)
							cleaned++
						}
					}
				}
				return st.Save()
			})
			if err != nil {
				return err
			}
			if cleaned == 0 {
				fmt.Fprintln(out, "Nothing to clean up.")
			} else {
				fmt.Fprintf(out, "✔ Cleaned up %d entr%s.\n", cleaned, pluralY(cleaned))
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

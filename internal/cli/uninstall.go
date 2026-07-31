package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/updater"
)

func newUninstallCmd() *cobra.Command {
	var keepFiles bool
	cmd := &cobra.Command{
		Use:     "uninstall <name>",
		Short:   "Unregister a repository and delete its clone",
		GroupID: "ext",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			out := cmd.OutOrStdout()

			st, err := state.Load()
			if err != nil {
				return err
			}
			repo, ok := st.Repos[name]
			if !ok {
				return fmt.Errorf("repository %q is not registered (see cepm list)", name)
			}

			// Offer the Chrome-side removal *before* unregistering: the host
			// only acts on extensions cepm currently manages, so doing it
			// afterwards would always be refused. This also runs outside the
			// update lock, since it waits for the user.
			candidates := append([]state.Extension(nil), repo.Extensions...)
			for _, s := range repo.Stale {
				candidates = append(candidates, state.Extension{Name: s.Name, ID: s.ID})
			}
			// With duplicate ids on record, an id does not identify one
			// extension, so removing it from Chrome could take another
			// registration's extension with it. Unregister anyway — that is
			// how the duplicate is resolved — but touch nothing in Chrome.
			gone := map[string]bool{}
			if err := st.Validate(); err != nil {
				fmt.Fprintf(out, "⚠ Not touching Chrome: %v\n", err)
				fmt.Fprintf(out, "  Unregistering %q anyway; run cepm cleanup afterwards.\n", name)
			} else {
				gone = offerChromeRemoval(cmd, candidates)
			}

			// Whatever is still in Chrome outlives its repository; keep a
			// record so cleanup can finish later.
			var orphans []state.StaleExtension
			for _, e := range candidates {
				if !gone[e.ID] {
					orphans = append(orphans, state.StaleExtension{
						ID: e.ID, Name: e.Name, Reason: "uninstalled",
					})
				}
			}

			err = updater.WithLock(cmd.Context(), func() error {
				st, err := state.Load()
				if err != nil {
					return err
				}
				if _, ok := st.Repos[name]; !ok {
					return fmt.Errorf("repository %q is no longer registered", name)
				}
				delete(st.Repos, name)
				st.AddOrphans(orphans)
				return st.Save()
			})
			if err != nil {
				return err
			}
			if !keepFiles {
				dir, err := updater.RepoDir(name)
				if err != nil {
					return err
				}
				if err := os.RemoveAll(dir); err != nil {
					return fmt.Errorf("unregistered, but failed to delete %s: %w", dir, err)
				}
			}
			fmt.Fprintf(out, "Uninstalled %q (%d extension(s)).\n", name, len(repo.Extensions))
			if len(orphans) > 0 {
				fmt.Fprintf(out, "%d entr%s still in Chrome; remove them there, or run: cepm cleanup\n",
					len(orphans), pluralY(len(orphans)))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&keepFiles, "keep-files", false, "keep the cloned directory on disk")
	return cmd
}

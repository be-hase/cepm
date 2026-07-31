package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/term"
	"github.com/be-hase/cepm/internal/updater"
)

// rebindTarget is an extension that survives an ambiguous id, together with
// the directory Chrome has to be pointed at.
type rebindTarget struct {
	Name   string
	ID     string
	AbsDir string
}

// rebindTargets finds the extensions of other repositories that share an id
// with the repository being removed. Those are exactly the ids where Chrome
// may be loading the wrong directory once this one is gone.
func rebindTargets(st *state.State, removing string, repo *state.Repo) []rebindTarget {
	mine := map[string]bool{}
	for _, e := range repo.Extensions {
		mine[e.ID] = true
	}
	var out []rebindTarget
	for _, name := range st.RepoNames() {
		if name == removing {
			continue
		}
		for _, e := range st.Repos[name].Extensions {
			if mine[e.ID] {
				out = append(out, rebindTarget{Name: e.Name, ID: e.ID, AbsDir: filepath.Join(dirOf(name), e.Dir)})
			}
		}
	}
	return out
}

// dirOf is the clone directory of a repository.
func dirOf(name string) string {
	dir, err := updater.RepoDir(name)
	if err != nil {
		return name
	}
	return dir
}

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
			var rebind []rebindTarget
			if invalid := st.Validate(); invalid != nil {
				fmt.Fprintf(out, "⚠ Not touching Chrome: %v\n", invalid)
				rebind = rebindTargets(st, name, repo)
				// Chrome may well be loading *this* clone: its id is claimed
				// by another registration too, and nothing can tell which
				// directory was loaded. Deleting it would leave Chrome
				// pointing at a path that no longer exists.
				keepFiles = true
			} else {
				gone = offerChromeRemoval(cmd, candidates)
			}

			// Whatever is still in Chrome outlives its repository; keep a
			// record so cleanup can finish later. Ids another registration
			// also claims are excluded: they stay live, so normalization
			// would drop the record and cleanup would have nothing to do —
			// promising otherwise would be a lie. Those are covered by the
			// re-load guidance instead.
			ambiguous := map[string]bool{}
			for _, rb := range rebind {
				ambiguous[rb.ID] = true
			}
			var orphans []state.StaleExtension
			for _, e := range candidates {
				if !gone[e.ID] && !ambiguous[e.ID] {
					orphans = append(orphans, state.StaleExtension{
						ID: e.ID, Name: e.Name, Reason: "uninstalled",
					})
				}
			}

			err = updater.WithLock(cmd.Context(), func() error {
				before, err := state.Load()
				if err != nil {
					return err
				}
				st, err := state.Load()
				if err != nil {
					return err
				}
				if _, ok := st.Repos[name]; !ok {
					return fmt.Errorf("repository %q is no longer registered", name)
				}
				delete(st.Repos, name)
				st.AddOrphans(orphans)
				if before.Validate() != nil {
					// Repairing a file with several independent collisions has
					// to work one repository at a time, so accept a state that
					// is still invalid as long as it is strictly better.
					return st.SaveRepair(before)
				}
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
			if len(rebind) > 0 {
				// The surviving owner is now the only registration for these
				// ids, but Chrome may still be running the copy from the
				// directory just unregistered. Only a manual re-load can tie
				// the id back to the right path.
				fmt.Fprintf(out, "\n⚠ Chrome may still be running the copy from %s\n", term.Quote(dirOf(name)))
				fmt.Fprintf(out, "  (its id was claimed twice, so cepm cannot tell which one you loaded).\n")
				fmt.Fprintf(out, "  Its files were kept for now. To finish:\n")
				for i, rb := range rebind {
					fmt.Fprintf(out, "   %d. In chrome://extensions, remove %q (id %s)\n", i*2+1, rb.Name, rb.ID)
					fmt.Fprintf(out, "   %d. Load unpacked %s\n", i*2+2, term.Quote(rb.AbsDir))
				}
				fmt.Fprintf(out, "  Then delete the old clone: rm -rf %s\n", term.Quote(dirOf(name)))
			}
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

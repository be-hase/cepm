package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/term"
	"github.com/be-hase/cepm/internal/updater"
)

// snapshot captures what a decision about a repository was based on, so a
// change made between reading it and acting can be noticed.
func snapshot(r *state.Repo) string {
	s := ""
	for _, e := range r.Extensions {
		s += e.Dir + "\x00" + e.ID + "\x00"
	}
	s += "|"
	for _, st := range r.Stale {
		s += st.ID + "\x00"
	}
	return s
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

			dir, err := updater.RepoDir(name)
			if err != nil {
				return err
			}

			// Ask first, act later. The question waits for a human, which the
			// update lock must not, so only the answer is collected here —
			// nothing in Chrome has changed yet if the state turns out to have
			// moved on.
			candidates := append([]state.Extension(nil), repo.Extensions...)
			for _, s := range repo.Stale {
				candidates = append(candidates, state.Extension{Name: s.Name, ID: s.ID})
			}
			before := snapshot(repo)
			approved := askChromeRemoval(cmd, candidates)

			var orphans []state.StaleExtension
			err = updater.WithLock(cmd.Context(), func() error {
				st, err := state.Load()
				if err != nil {
					return err
				}
				fresh, ok := st.Repos[name]
				if !ok {
					return fmt.Errorf("repository %q is no longer registered", name)
				}
				if snapshot(fresh) != before {
					// An update ran while we were asking: the extensions we
					// asked about are not the ones registered now. Nothing has
					// been touched yet, so stopping here really does undo it.
					return fmt.Errorf("%q changed while waiting for your answer; nothing was changed — run cepm uninstall %s again",
						name, term.Quote(name))
				}
				// Removing from Chrome happens here, where the state cannot
				// change under it — the same trade cleanup makes.
				gone := performChromeRemoval(cmd, candidates, approved)
				orphans = nil
				for _, e := range candidates {
					if !gone[e.ID] {
						orphans = append(orphans, state.StaleExtension{
							ID: e.ID, Name: e.Name, Reason: "uninstalled",
						})
					}
				}
				delete(st.Repos, name)
				st.AddOrphans(orphans)
				return st.Save()
			})
			if err != nil {
				return err
			}
			// Only once the state is safely on disk: a failed save with the
			// files already gone would leave a repository registered with
			// nothing behind it.
			if !keepFiles {
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

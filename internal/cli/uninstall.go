package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/assist"
	"github.com/be-hase/cepm/internal/gitx"
	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/term"
	"github.com/be-hase/cepm/internal/updater"
)

// snapshot captures what a decision about a repository was based on, so a
// change made between reading it and acting can be noticed. The disabled
// bit is part of it: an enable answered mid-prompt changes what a removal
// would mean.
func snapshot(r *state.Repo) string {
	s := ""
	for _, e := range r.Extensions {
		s += e.Dir + "\x00" + e.ID + "\x00" + fmt.Sprint(e.Disabled) + "\x00"
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

			// Local edits are precious everywhere else (update skips dirty
			// trees; stash-pop failures halt with recovery steps), so they
			// must not vanish silently here either. An IsDirty error is left
			// alone: the clone may be gone or corrupt, which uninstall exists
			// to clean up.
			if !keepFiles {
				if dirty, derr := (gitx.Repo{Dir: dir}).IsDirty(cmd.Context()); derr == nil && dirty {
					if !assist.IsTTY() {
						return fmt.Errorf("%s has uncommitted local changes; commit or stash them first, or re-run with --keep-files to keep the clone", dir)
					}
					if !confirm(cmd, fmt.Sprintf("%s has uncommitted local changes — delete them anyway?", dir)) {
						return errors.New("aborted; nothing was changed (--keep-files uninstalls but keeps the clone)")
					}
				}
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

			changed := func(fresh *state.Repo, ok bool) error {
				if !ok {
					return fmt.Errorf("repository %q is no longer registered", name)
				}
				if snapshot(fresh) != before {
					// An update ran while we were asking: the extensions we
					// asked about are not the ones registered now.
					return fmt.Errorf("%q changed while waiting for your answer; run cepm uninstall %s again",
						name, term.Quote(name))
				}
				return nil
			}

			// Chrome-side removal, one entry at a time, each holding the lock
			// only for its own dialog: holding it across the batch scales with
			// the number of dialogs, and three unanswered ones already exceed
			// the five minutes every other cepm waits (see cleanup). Each
			// entry re-checks the state under its own lock; removals already
			// confirmed through Chrome's dialog stay done if a later entry
			// aborts.
			for _, e := range candidates {
				if !approved[e.ID] {
					continue
				}
				err := updater.WithLock(cmd.Context(), func() error {
					st, err := state.Load()
					if err != nil {
						return err
					}
					fresh, ok := st.Repos[name]
					if err := changed(fresh, ok); err != nil {
						return err
					}
					performChromeRemoval(cmd, []state.Extension{e}, approved)
					return nil
				})
				if err != nil {
					return err
				}
			}

			var orphans []state.StaleExtension
			err = updater.WithLock(cmd.Context(), func() error {
				st, err := state.Load()
				if err != nil {
					return err
				}
				fresh, ok := st.Repos[name]
				if err := changed(fresh, ok); err != nil {
					return err
				}
				// What is still loaded in Chrome decides who needs an orphan
				// record. When Chrome cannot be listed nothing is presumed
				// absent, so every id stays covered by a record.
				listCtx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
				loaded, lerr := ipc.ListChrome(listCtx)
				cancel()
				present := map[string]bool{}
				for _, x := range loaded {
					present[x.ID] = true
				}
				orphans = nil
				for _, e := range candidates {
					if lerr != nil || present[e.ID] {
						orphans = append(orphans, state.StaleExtension{
							ID: e.ID, Name: e.Name, Reason: "uninstalled",
						})
					}
				}
				delete(st.Repos, name)
				st.AddOrphans(orphans)
				return saveState(cmd.OutOrStdout(), st)
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

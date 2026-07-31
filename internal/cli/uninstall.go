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

// rebindTarget describes what has to happen to one ambiguous id after this
// repository is unregistered.
type rebindTarget struct {
	ID   string
	Name string
	// AbsDir is the directory to load, set only when exactly one
	// registration survives and the user wants it enabled.
	AbsDir string
	// Unresolved is true while more than one registration still claims the
	// id: nothing can be re-pointed until the rest are uninstalled.
	Unresolved bool
	// DisabledOwner is true when the single survivor is one the user chose
	// not to use — telling them to load it would undo that.
	DisabledOwner bool
}

// rebindTargets works out, per shared id, what the user has to do once this
// repository is gone. Grouping by id matters: with three claimants, removing
// one leaves two, and guidance naming both would ask the user to load two
// directories under a single id.
func rebindTargets(st *state.State, removing string, repo *state.Repo) []rebindTarget {
	mine := map[string]bool{}
	for _, e := range repo.Extensions {
		mine[e.ID] = true
	}
	survivors := map[string][]state.Extension{}
	owner := map[string]string{}
	for _, name := range st.RepoNames() {
		if name == removing {
			continue
		}
		for _, e := range st.Repos[name].Extensions {
			if mine[e.ID] {
				survivors[e.ID] = append(survivors[e.ID], e)
				owner[e.ID] = name
			}
		}
	}
	var out []rebindTarget
	seen := map[string]bool{}
	for _, e := range repo.Extensions {
		rest, shared := survivors[e.ID]
		if !shared || seen[e.ID] {
			continue // not contested, or two of our own dirs claim it
		}
		seen[e.ID] = true
		switch {
		case len(rest) > 1:
			out = append(out, rebindTarget{ID: e.ID, Name: e.Name, Unresolved: true})
		case rest[0].Enabled():
			out = append(out, rebindTarget{ID: e.ID, Name: rest[0].Name,
				AbsDir: filepath.Join(dirOf(owner[e.ID]), rest[0].Dir)})
		default:
			out = append(out, rebindTarget{ID: e.ID, Name: rest[0].Name, DisabledOwner: true})
		}
	}
	return out
}

// allResolved reports whether every ambiguous id now has a single owner.
func allResolved(targets []rebindTarget) bool {
	for _, t := range targets {
		if t.Unresolved {
			return false
		}
	}
	return true
}

// dirOf is the clone directory of a repository.
func dirOf(name string) string {
	dir, err := updater.RepoDir(name)
	if err != nil {
		return name
	}
	return dir
}

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

			if st.Validate() != nil {
				// Repair: nothing is asked of Chrome, so there is no prompt to
				// wait on and every decision can be made from the state read
				// under the lock.
				return repairUninstall(cmd, name, dir, keepFiles)
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
			approved, gone := askChromeRemoval(cmd, candidates)

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
				performChromeRemoval(cmd, candidates, approved, gone)
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

// repairUninstall unregisters a repository from a state cepm will not act on
// — duplicate ids, or a directory name it cannot print. Chrome is left alone
// throughout: an id claimed twice does not identify one extension, so
// removing it could take the other registration's extension with it.
func repairUninstall(cmd *cobra.Command, name, dir string, keepFiles bool) error {
	out := cmd.OutOrStdout()
	var (
		rebind     []rebindTarget
		kept       bool
		resolved   []string
		extensions int
	)
	err := updater.WithLock(cmd.Context(), func() error {
		before, err := state.Load()
		if err != nil {
			return err
		}
		if invalid := before.Validate(); invalid != nil {
			fmt.Fprintf(out, "⚠ Not touching Chrome: %v\n", invalid)
		} else {
			// Someone repaired it between our read and this lock. The normal
			// path asks about Chrome, which this one deliberately skips, so
			// do not quietly unregister a repository under repair rules.
			return fmt.Errorf("the state was repaired by another cepm; run cepm uninstall %s again", term.Quote(name))
		}
		st, err := state.Load()
		if err != nil {
			return err
		}
		repo, ok := st.Repos[name]
		if !ok {
			return fmt.Errorf("repository %q is no longer registered", name)
		}
		extensions = len(repo.Extensions)

		rebind = rebindTargets(st, name, repo)
		// Ids another registration also claims stay live, so a record for
		// them would be dropped by normalization and cleanup would have
		// nothing to do. Those are covered by the re-load guidance instead.
		ambiguous := map[string]bool{}
		for _, rb := range rebind {
			ambiguous[rb.ID] = true
		}
		seen := map[string]bool{}
		add := func(id, extName string) {
			if ambiguous[id] || seen[id] {
				return
			}
			seen[id] = true
			st.AddOrphans([]state.StaleExtension{{ID: id, Name: extName, Reason: "uninstalled"}})
		}
		for _, e := range repo.Extensions {
			add(e.ID, e.Name)
		}
		// Entries this repository had already lost track of are still in
		// Chrome; losing their records with the repository would put them
		// beyond cleanup's reach for good.
		for _, s := range repo.Stale {
			add(s.ID, s.Name)
		}
		delete(st.Repos, name)

		// Keep the clone only while Chrome might be loading it, which is
		// exactly when another registration shares one of its ids.
		if len(rebind) > 0 {
			st.KeepClone(dir)
			kept = true
		}
		if st.Validate() == nil {
			// Fixed: the clones kept along the way can go once the user has
			// re-pointed Chrome, so hand them back to be reported.
			resolved = st.TakeKeptClones()
			return st.Save()
		}
		return st.SaveRepair(before)
	})
	if err != nil {
		return err
	}
	// After the state is on disk, and never against a path we could not
	// resolve properly.
	if !kept && !keepFiles {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("unregistered, but failed to delete %s: %w", dir, err)
		}
	}

	fmt.Fprintf(out, "Uninstalled %q (%d extension(s)).\n", name, extensions)
	if kept {
		fmt.Fprintf(out, "\n⚠ Chrome may still be running the copy from %s\n", term.Quote(dir))
		fmt.Fprintf(out, "  (its id was claimed more than once, so cepm cannot tell which one you loaded).\n")
		fmt.Fprintf(out, "  Its files were kept for now.\n")
	}
	for _, rb := range rebind {
		switch {
		case rb.Unresolved:
			fmt.Fprintf(out, "  • id %s is still claimed by more than one repository;\n", rb.ID)
			fmt.Fprintf(out, "    uninstall the others first, then re-run to get the steps.\n")
		case rb.DisabledOwner:
			fmt.Fprintf(out, "  • id %s: the remaining registration (%q) is one you chose not to use.\n", rb.ID, rb.Name)
			fmt.Fprintf(out, "    Remove it in chrome://extensions; enable it with cepm enable if you want it back.\n")
		default:
			fmt.Fprintf(out, "  • id %s: remove %q in chrome://extensions, then Load unpacked %s\n",
				rb.ID, rb.Name, term.Quote(rb.AbsDir))
		}
	}
	if len(resolved) > 0 && allResolved(rebind) {
		fmt.Fprintf(out, "\n✔ No id is contested any more. Once Chrome is pointed at the right\n")
		fmt.Fprintf(out, "  directories, these leftover clones can go:\n")
		for _, d := range resolved {
			fmt.Fprintf(out, "    rm -rf %s\n", term.Quote(d))
		}
	}
	return nil
}

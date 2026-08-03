package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/gitx"
	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/paths"
	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/term"
	"github.com/be-hase/cepm/internal/updater"
)

// snapshot captures what a decision about a repository was based on, so a
// change made between reading it and acting can be noticed. The disabled
// bit is part of it: an enable answered mid-prompt changes what a removal
// would mean. So is the registration's provenance — URL and tracking: a
// reset plus an install can put a *different* repository under the same
// name with the same path-derived ids while a dialog is open, and acting
// on it then would carry an answer about one registration onto another.
// Head is deliberately absent: the periodic updater moves it, and an
// update does not change which registration this is.
func snapshot(r *state.Repo) string {
	s := r.URL + "\x00" + r.Track + "\x00" + r.Branch + "\x00" +
		r.TagPattern + "\x00" + r.Tag + "\x00" + fmt.Sprint(r.Prerelease) + "\x00|"
	for _, e := range r.Extensions {
		s += e.Dir + "\x00" + e.ID + "\x00" + fmt.Sprint(e.Disabled) + "\x00"
	}
	s += "|"
	for _, st := range r.Stale {
		s += st.ID + "\x00"
	}
	return s
}

// stateGeneration identifies the state file *instance*, so that two
// registrations alike in every attribute are still told apart: a reset plus
// a re-install of the same URL reproduces every attribute, but both pass
// through state.Save, and Save replaces state.json by rename — new inode,
// new mtime, new size. The clone directory's own inode is useless for this:
// ext4 reuses a just-freed inode immediately (the Linux CI proved it), and
// nothing recreates state.json in place the way a directory can be.
// Comparing generations therefore answers "did anyone save state while the
// dialogs were open" — including a periodic update, which is also a state
// this command's answers were not about.
func stateGeneration() string {
	path, err := paths.StateFile()
	if err != nil {
		return ""
	}
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	dev, _ := deviceOf(fi)
	ino, _ := inodeOf(fi)
	return fmt.Sprintf("%d:%d:%d:%d", dev, ino, fi.ModTime().UnixNano(), fi.Size())
}

// trashClone moves a clone into a freshly created trash directory and
// returns where it went. A move, never a delete, on principle: everything
// else in cepm that could destroy a user's work refuses to (stash entries
// are never dropped, reset renames into a backup), and deleting here would
// otherwise demand proving that nothing in the clone changed between the
// user's answer and the delete — an arms race against every place git can
// hold work. A rename needs no such proof: whatever changed is still there.
//
// The trash lives next to the staging area (see stagingParentFor): normally
// the cepm home, but inside repos/ when that is another filesystem, because
// a rename cannot cross devices. Dot-prefixed so nothing that scans
// directories mistakes it for content.
//
// A variable so a test can prove the move happens while the update lock is
// held — between a save and a move outside the lock, a reset plus an
// install could put a brand-new clone at this same logical path.
var trashClone = func(dir, name string) (string, error) {
	home, err := paths.CepmDir()
	if err != nil {
		return "", err
	}
	reposDir, err := paths.ReposDir()
	if err != nil {
		return "", err
	}
	parent := stagingParentFor(home, reposDir)
	if resolved, rerr := filepath.EvalSymlinks(parent); rerr == nil {
		parent = resolved
	}
	trash, err := os.MkdirTemp(parent, ".trash-")
	if err != nil {
		return "", err
	}
	dst := filepath.Join(trash, name)
	if err := os.Rename(dir, dst); err != nil {
		_ = os.Remove(trash)
		return "", err
	}
	return dst, nil
}

func newUninstallCmd() *cobra.Command {
	var keepFiles bool
	cmd := &cobra.Command{
		Use:     "uninstall <name>",
		Short:   "Unregister a repository and move its clone to the trash",
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

			// For the report only. Local edits are precious everywhere else
			// in cepm, and here they need no gate at all: the clone is
			// moved, not deleted, so nothing that happens between this look
			// and the move can be lost. "Cannot tell" points the caution
			// the same way as "dirty".
			dirty, derr := (gitx.Repo{Dir: dir}).IsDirty(cmd.Context())
			mentionChanges := derr != nil || dirty

			// Ask first, act later. The question waits for a human, which the
			// update lock must not, so only the answer is collected here —
			// nothing in Chrome has changed yet if the state turns out to have
			// moved on.
			candidates := append([]state.Extension(nil), repo.Extensions...)
			for _, s := range repo.Stale {
				candidates = append(candidates, state.Extension{Name: s.Name, ID: s.ID})
			}
			before := snapshot(repo) + "\x00" + stateGeneration()
			approved := askChromeRemoval(cmd, candidates)

			changed := func(fresh *state.Repo, ok bool) error {
				if !ok {
					return fmt.Errorf("repository %q is no longer registered", name)
				}
				if snapshot(fresh)+"\x00"+stateGeneration() != before {
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
			trashed := ""
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
				// A cancelled caller stops before anything is written: the
				// Ctrl-C was pressed to stop this, not to hurry it.
				if cerr := cmd.Context().Err(); cerr != nil {
					return cerr
				}
				delete(st.Repos, name)
				st.AddOrphans(orphans)
				if err := st.Save(); err != nil {
					// With the clone about to be moved, "the new state is
					// live" is not enough: a crash before the flush lands
					// brings the registration back with its clone already
					// elsewhere. Keeping the files has no such step, so
					// there a durability-only failure must not report an
					// uninstall that did happen as failed.
					if keepFiles && state.Committed(err) {
						fmt.Fprintf(out, "Warning: %v\n", err)
						return nil
					}
					return err
				}
				// Moving inside the lock, deliberately. Between a save and
				// a move outside it, a reset plus an install can put a
				// brand-new clone at this same logical path — and the move
				// would then take a clone belonging to a registration this
				// command never saw.
				if !keepFiles {
					dst, err := trashClone(dir, name)
					if err != nil {
						return fmt.Errorf("unregistered, but could not move %s to the trash: %w — remove it yourself", dir, err)
					}
					trashed = dst
				}
				return nil
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Uninstalled %q (%d extension(s)).\n", name, len(repo.Extensions))
			if trashed != "" {
				fmt.Fprintf(out, "The clone was moved to %s — delete it whenever you like.\n", term.Quote(trashed))
				if mentionChanges {
					fmt.Fprintf(out, "  (it contains, or may contain, uncommitted local changes)\n")
				}
			}
			if len(orphans) > 0 {
				fmt.Fprintf(out, "%d entr%s still in Chrome; remove them there, or run: cepm cleanup\n",
					len(orphans), pluralY(len(orphans)))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&keepFiles, "keep-files", false, "keep the cloned directory where it is")
	return cmd
}

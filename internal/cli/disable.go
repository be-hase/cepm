package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/assist"
	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/updater"
)

func newDisableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disable <repo>[/<dir>]",
		Short: "Stop managing an extension (keeps it registered as available)",
		Long: `Disable marks an extension as unused: it is skipped by update reloads and
doctor stops expecting it in Chrome. If it is still loaded, cepm offers to
remove it from Chrome too (Chrome shows its own confirmation dialog).`,
		GroupID: "ext",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repoName, dir := parseExtRef(args[0])
			// Choose and ask before locking (see the note in cepm enable):
			// both wait for a human, which the lock must not.
			st, err := state.Load()
			if err != nil {
				return err
			}
			r, ok := st.Repos[repoName]
			if !ok {
				return fmt.Errorf("repository %q is not registered (see cepm list)", repoName)
			}
			picked, err := pickExtensions(cmd, r, dir, true)
			if err != nil {
				return err
			}
			dirs := make([]string, len(picked))
			asked := make([]state.Extension, len(picked))
			for i, e := range picked {
				dirs[i] = e.Dir
				asked[i] = *e
			}
			before := snapshot(r) + "\x00" + stateGeneration()
			approved := askChromeRemoval(cmd, asked)

			var disabled []state.Extension
			err = updater.WithLock(cmd.Context(), func() error {
				st, err := state.Load()
				if err != nil {
					return err
				}
				r, ok := st.Repos[repoName]
				if !ok {
					return fmt.Errorf("repository %q is no longer registered", repoName)
				}
				if snapshot(r)+"\x00"+stateGeneration() != before {
					// Another cepm ran while we were asking: the extensions
					// we asked about are not the ones registered now. Nothing
					// has been touched yet, so stopping here really does undo
					// it.
					return fmt.Errorf("%q changed while waiting for your answer; nothing was changed — run cepm disable again", repoName)
				}
				for _, d := range dirs {
					e := r.FindExtension(d)
					if e == nil {
						return fmt.Errorf("extension %q is no longer part of %s", d, repoName)
					}
					e.Disabled = true
					disabled = append(disabled, *e)
				}
				return saveState(cmd.OutOrStdout(), st)
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			names := make([]string, len(disabled))
			for i, e := range disabled {
				names[i] = e.Name
			}
			fmt.Fprintf(out, "✔ Disabled: %s\n", strings.Join(names, ", "))

			// Chrome-side removal, one entry at a time, each holding the lock
			// only for its own dialog (see cleanup for why not the batch) —
			// and re-decided under it: an enable in another terminal since the
			// prompt means the extension is wanted again, and removing it then
			// would override that choice.
			for _, e := range disabled {
				if !approved[e.ID] {
					continue
				}
				err := updater.WithLock(cmd.Context(), func() error {
					st, err := state.Load()
					if err != nil {
						return err
					}
					r, ok := st.Repos[repoName]
					if !ok {
						// Uninstalled meanwhile; that flow owns the Chrome side.
						return nil
					}
					fresh := r.FindExtension(e.Dir)
					if fresh == nil || fresh.ID != e.ID || !fresh.Disabled {
						fmt.Fprintf(out, "  %q is wanted again; leaving Chrome alone\n", e.Name)
						return nil
					}
					performChromeRemoval(cmd, []state.Extension{e}, approved)
					return nil
				})
				if err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// askChromeRemoval asks which of the given extensions the user wants removed
// from Chrome. It only asks — nothing is changed — so a caller can still
// abandon the whole operation after finding the world moved underneath it.
// What it learns about Chrome is used only to decide what to ask; whether an
// extension is actually present is re-checked at removal time.
func askChromeRemoval(cmd *cobra.Command, exts []state.Extension) (approved map[string]bool) {
	out := cmd.OutOrStdout()
	approved = map[string]bool{}
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
	loaded, err := ipc.ListChrome(ctx)
	cancel()
	if err != nil {
		// Chrome unreachable: say so rather than leaving broken entries
		// behind silently.
		if len(exts) > 0 {
			fmt.Fprintf(out, "  Chrome is not reachable; %d entr%s may still be loaded there.\n",
				len(exts), pluralY(len(exts)))
		}
		return approved
	}
	loadedSet := map[string]bool{}
	for _, e := range loaded {
		loadedSet[e.ID] = true
	}
	for _, e := range exts {
		if !loadedSet[e.ID] {
			continue // nothing to ask about
		}
		if !assist.IsTTY() {
			fmt.Fprintf(out, "  %q is still loaded in Chrome — remove it there if unwanted.\n", e.Name)
			continue
		}
		if confirm(cmd, fmt.Sprintf("%q is still loaded in Chrome — remove it there too?", e.Name)) {
			approved[e.ID] = true
		}
	}
	return approved
}

// performChromeRemoval carries out the removals the user approved and reports
// which ids are absent from Chrome afterwards. Chrome shows its own
// confirmation for each. Presence is judged fresh here, not at the prompt:
// the answer may be minutes old, and an extension absent then can have been
// loaded since (a cepm enable in another terminal) — treating it as still
// gone would delete its files while Chrome keeps loading them, with no
// orphan record for cleanup to find. When Chrome cannot be listed nothing is
// presumed absent, so every id stays covered by a record.
func performChromeRemoval(cmd *cobra.Command, exts []state.Extension, approved map[string]bool) (gone map[string]bool) {
	gone = map[string]bool{}
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
	loaded, err := ipc.ListChrome(ctx)
	cancel()
	loadedNow := map[string]bool{}
	for _, e := range loaded {
		loadedNow[e.ID] = true
	}
	for _, e := range exts {
		if err == nil && !loadedNow[e.ID] {
			gone[e.ID] = true
			continue
		}
		if !approved[e.ID] {
			continue
		}
		if uninstallViaChrome(cmd.Context(), cmd, e.ID, e.Name) {
			gone[e.ID] = true
		}
	}
	return gone
}

// uninstallViaChrome triggers Chrome's uninstall confirmation dialog for the
// extension and reports the outcome, returning whether it is gone from Chrome.
func uninstallViaChrome(ctx context.Context, cmd *cobra.Command, id, name string) bool {
	out := cmd.OutOrStdout()
	// Deliberately no deadline. The wait is a person answering Chrome's
	// dialog, and nothing closes that dialog when a deadline expires — so a
	// deadline here would only release the update lock while the "Remove"
	// button is still live, which is the thing the lock is held across. The
	// host stops waiting when the helper goes quiet, and Ctrl-C reaches the
	// read (see ipc.watchCancel), so this cannot hang forever.
	fmt.Fprintf(out, "  Asking Chrome to uninstall %q (confirm in the dialog Chrome shows) ... ", name)
	status, err := ipc.Uninstall(ctx, id)
	switch {
	case errors.Is(err, ipc.ErrHostNotRunning):
		fmt.Fprintln(out, "Chrome is not reachable; remove it manually in chrome://extensions")
	case err != nil:
		fmt.Fprintf(out, "failed: %v\n", err)
	case status == ipc.StatusUninstalled:
		fmt.Fprintln(out, "✔ removed")
		return true
	case status == ipc.StatusCancelled:
		fmt.Fprintln(out, "cancelled")
	case status == ipc.StatusNotInstalled:
		fmt.Fprintln(out, "already gone")
		return true
	default:
		fmt.Fprintln(out, status)
	}
	return false
}

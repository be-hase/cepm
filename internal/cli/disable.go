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
			// Choose before locking; see the note in cepm enable.
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
			for i, e := range picked {
				dirs[i] = e.Dir
			}

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
				for _, d := range dirs {
					e := r.FindExtension(d)
					if e == nil {
						return fmt.Errorf("extension %q is no longer part of %s", d, repoName)
					}
					e.Disabled = true
					disabled = append(disabled, *e)
				}
				return st.Save()
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
			offerChromeRemoval(cmd, disabled)
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

// offerChromeRemoval asks and then acts, for callers that have already
// committed their state change.
func offerChromeRemoval(cmd *cobra.Command, exts []state.Extension) map[string]bool {
	approved := askChromeRemoval(cmd, exts)
	return performChromeRemoval(cmd, exts, approved)
}

// uninstallViaChrome triggers Chrome's uninstall confirmation dialog for the
// extension and reports the outcome, returning whether it is gone from Chrome.
func uninstallViaChrome(ctx context.Context, cmd *cobra.Command, id, name string) bool {
	out := cmd.OutOrStdout()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
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

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/config"
	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/updater"
)

func newUpdateCmd() *cobra.Command {
	var (
		noReload bool
		force    bool
	)
	cmd := &cobra.Command{
		Use:     "update [name...]",
		Short:   "Pull all (or the named) repositories and reload changed extensions",
		GroupID: "ext",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			opts := updater.Options{StashDirty: cfg.Git.StashDirty || force}
			results, err := updater.Update(cmd.Context(), args, opts)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			failed := printUpdateResults(out, results)

			var reloadIDs []string
			var reloadNames []string
			for _, r := range results {
				for _, c := range r.Changed {
					reloadIDs = append(reloadIDs, c.ID)
					reloadNames = append(reloadNames, c.Name)
				}
			}
			if len(reloadIDs) > 0 && !noReload {
				reloadExtensions(cmd.Context(), out, reloadIDs, reloadNames)
			}
			if failed {
				return errors.New("some repositories failed to update (see above)")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noReload, "no-reload", false, "pull only; do not reload extensions in Chrome")
	cmd.Flags().BoolVar(&force, "force", false, "stash local changes before pulling dirty repositories")
	return cmd
}

// printUpdateResults renders per-repo outcomes and reports whether any failed.
func printUpdateResults(out io.Writer, results []updater.RepoResult) (failed bool) {
	for _, r := range results {
		switch {
		case r.Err != nil:
			failed = true
			fmt.Fprintf(out, "✘ %-16s %v\n", r.Name, r.Err)
		case r.Skipped:
			fmt.Fprintf(out, "- %-16s skipped: %s\n", r.Name, r.SkipReason)
		case !r.Updated:
			fmt.Fprintf(out, "✔ %-16s up to date (%s)\n", r.Name, r.NewRef)
		default:
			fmt.Fprintf(out, "✔ %-16s %s → %s (%d extension(s) changed)\n",
				r.Name, r.OldRef, r.NewRef, len(r.Changed))
		}
		for _, w := range r.Warnings {
			fmt.Fprintf(out, "  ⚠ %s\n", w)
		}
		for _, a := range r.Added {
			fmt.Fprintf(out, "  + new extension %q — one-time step: Load unpacked %s\n", a.Name, a.AbsDir)
		}
		for _, rm := range r.Removed {
			fmt.Fprintf(out, "  - extension %q was removed from the repo; remove it from chrome://extensions\n", rm.Name)
		}
	}
	return failed
}

// reloadExtensions asks the native host to reload IDs, degrading gracefully
// when Chrome is not running.
func reloadExtensions(ctx context.Context, out io.Writer, ids, names []string) {
	results, err := ipc.Reload(ctx, ids)
	if errors.Is(err, ipc.ErrHostNotRunning) {
		fmt.Fprintf(out, "\nChrome is not running (or the helper is not connected); updates will apply on next launch.\n")
		return
	}
	if err != nil {
		fmt.Fprintf(out, "\nReload failed: %v\nExtensions will pick up changes on next Chrome launch, or run: cepm reload\n", err)
		return
	}
	byID := map[string]ipc.ReloadResult{}
	for _, r := range results {
		byID[r.ID] = r
	}
	fmt.Fprintln(out)
	for i, id := range ids {
		name := id
		if i < len(names) && names[i] != "" {
			name = names[i]
		}
		switch r := byID[id]; r.Status {
		case ipc.StatusReloaded:
			fmt.Fprintf(out, "↻ reloaded %s\n", name)
		case ipc.StatusNotInstalled:
			fmt.Fprintf(out, "  %s is not loaded in Chrome (one-time \"Load unpacked\" still needed)\n", name)
		default:
			fmt.Fprintf(out, "✘ reload %s failed: %s\n", name, r.Error)
		}
	}
}

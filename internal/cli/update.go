package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

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

			var items []reloadItem
			for _, r := range results {
				for _, c := range r.Changed {
					items = append(items, reloadItem{ID: c.ID, Name: c.Name, ManifestChanged: c.ManifestChanged})
				}
			}
			if len(items) > 0 && !noReload {
				reloadExtensions(cmd.Context(), out, items)
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
			fmt.Fprintf(out, "  + new extension available: %q — enable with: cepm enable %s/%s\n", a.Name, r.Name, a.Dir)
		}
		for _, rn := range r.Renamed {
			fmt.Fprintf(out, "  ~ %q moved: %s → %s\n", rn.Name, rn.OldDir, rn.NewDir)
			if rn.Enabled {
				fmt.Fprintf(out, "    Chrome cannot follow a path change — one-time re-load needed:\n")
				fmt.Fprintf(out, "      1. Load unpacked the new dir: %s\n", rn.AbsDir)
				fmt.Fprintf(out, "      2. Remove the old broken entry — run: cepm cleanup\n")
			}
		}
		for _, rm := range r.Removed {
			fmt.Fprintf(out, "  - %q was removed from the repo; its loaded copy is now broken — run: cepm cleanup\n", rm.Name)
		}
	}
	return failed
}

// reloadItem is one extension to reload.
type reloadItem struct {
	ID              string
	Name            string
	ManifestChanged bool
}

// reloadExtensions asks the native host to reload the given extensions,
// degrading gracefully when Chrome is not running.
func reloadExtensions(ctx context.Context, out io.Writer, items []reloadItem) {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
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
	var manifestPending []string
	for _, it := range items {
		name := it.Name
		if name == "" {
			name = it.ID
		}
		r, answered := byID[it.ID]
		switch {
		case !answered:
			fmt.Fprintf(out, "? %s: no reload result from Chrome; check with cepm doctor\n", name)
		case r.Status == ipc.StatusReloaded:
			fmt.Fprintf(out, "↻ reloaded %s\n", name)
			if it.ManifestChanged {
				manifestPending = append(manifestPending, name)
			}
		case r.Status == ipc.StatusNotInstalled:
			fmt.Fprintf(out, "  %s is not loaded in Chrome (one-time \"Load unpacked\" still needed)\n", name)
		case r.Status == ipc.StatusSkippedDisabled:
			fmt.Fprintf(out, "  %s is turned off in Chrome; left as is\n", name)
		default:
			fmt.Fprintf(out, "✘ reload %s failed: %s\n", name, r.Error)
		}
	}
	if len(manifestPending) > 0 {
		fmt.Fprintf(out, "\n⚠ manifest.json changed for: %s\n", strings.Join(manifestPending, ", "))
		fmt.Fprintf(out, "  Code changes are live, but Chrome keeps the manifest it cached at install\n")
		fmt.Fprintf(out, "  time. Restart Chrome (or click Reload in chrome://extensions) to apply\n")
		fmt.Fprintf(out, "  manifest changes such as new permissions or a version bump.\n")
	}
}

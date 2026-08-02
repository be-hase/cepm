package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/assist"
	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/term"
	"github.com/be-hase/cepm/internal/updater"
)

// parseExtRef splits "<repo>[/<dir>]" (repo names cannot contain "/").
func parseExtRef(arg string) (repo, dir string) {
	parts := strings.SplitN(arg, "/", 2)
	if len(parts) == 2 {
		return parts[0], filepath.Clean(parts[1])
	}
	return parts[0], ""
}

func newEnableCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "enable <repo>[/<dir>]",
		Short: "Enable an extension of a repo (and walk through loading it)",
		Long: `Enable marks an extension as one you use: it becomes a reload target for
updates and doctor watches that it is actually loaded in Chrome. Without
<dir>, cepm picks the extension interactively (or automatically when only
one candidate exists).`,
		GroupID: "ext",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repoName, dir := parseExtRef(args[0])
			// Choose before locking: prompting with the update lock held
			// would stall the background updater for as long as the user
			// takes to answer.
			st, err := state.Load()
			if err != nil {
				return err
			}
			r, ok := st.Repos[repoName]
			if !ok {
				return fmt.Errorf("repository %q is not registered (see cepm list)", repoName)
			}
			picked, err := pickExtensions(cmd, r, dir, false)
			if err != nil {
				return err
			}
			dirs := make([]string, len(picked))
			for i, e := range picked {
				dirs[i] = e.Dir
			}

			var targets []loadTarget
			err = updater.WithLock(cmd.Context(), func() error {
				st, err := state.Load() // re-read: it may have changed while we prompted
				if err != nil {
					return err
				}
				r, ok := st.Repos[repoName]
				if !ok {
					return fmt.Errorf("repository %q is no longer registered", repoName)
				}
				repoDir, err := updater.RepoDir(repoName)
				if err != nil {
					return err
				}
				for _, d := range dirs {
					e := r.FindExtension(d)
					if e == nil {
						return fmt.Errorf("extension %q is no longer part of %s", d, repoName)
					}
					e.Disabled = false
					targets = append(targets, loadTarget{
						Name: e.Name, AbsDir: filepath.Join(repoDir, e.Dir), ID: e.ID,
					})
				}
				return st.Save()
			})
			if err != nil {
				return err
			}
			names := make([]string, len(targets))
			for i, t := range targets {
				names[i] = t.Name
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "✔ Enabled: %s\n", strings.Join(names, ", "))

			// Code pulled while the extension sat disabled is already on
			// disk, so one still loaded in Chrome gets a reload now instead
			// of running stale code until the next update tick. The rest go
			// through the load ceremony.
			listCtx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
			loaded, lerr := ipc.ListChrome(listCtx)
			cancel()
			var unconfirmed int
			if lerr == nil {
				byID := map[string]loadTarget{}
				loadedSet := map[string]bool{}
				for _, e := range loaded {
					loadedSet[e.ID] = true
				}
				var items []reloadItem
				var rest []loadTarget
				for _, t := range targets {
					byID[t.ID] = t
					if loadedSet[t.ID] {
						items = append(items, reloadItem{ID: t.ID, Name: t.Name})
					} else {
						rest = append(rest, t)
					}
				}
				if len(items) > 0 {
					outcome := reloadExtensions(cmd.Context(), out, items)
					// Gone from Chrome between the list and the reload: it
					// still needs the one-time Load unpacked after all.
					for _, id := range outcome.NotInstalledIDs {
						rest = append(rest, byID[id])
					}
					// Everything else short of an actual reload leaves the
					// extension registered but running old code (or switched
					// off) in Chrome — "enable" has not finished.
					if outcome.HostUnreachable {
						unconfirmed = len(items)
					} else {
						unconfirmed = outcome.Failed + outcome.Skipped
					}
				}
				targets = rest
			}
			runLoadCeremony(cmd.Context(), cmd, targets)
			if unconfirmed > 0 {
				// The state stays enabled on purpose: the user's intent was
				// recorded and re-running enable would change nothing. What
				// is unfinished is Chrome's side.
				fmt.Fprintf(out, "\n%d extension(s) are enabled in cepm but not confirmed live in Chrome.\n", unconfirmed)
				fmt.Fprintf(out, "  Retry with: cepm reload %s\n", term.Quote(args[0]))
				fmt.Fprintf(out, "  If Chrome shows them turned off, switch them on in chrome://extensions;\n")
				fmt.Fprintf(out, "  restarting Chrome also applies what is on disk.\n")
				return fmt.Errorf("%d extension(s) could not be confirmed live in Chrome (see above)", unconfirmed)
			}
			return nil
		},
	}
}

// pickExtensions resolves which extensions of r the user means. dir may be
// empty; disabled selects from currently-enabled ones (for cepm disable) or
// currently-disabled ones (for cepm enable).
func pickExtensions(cmd *cobra.Command, r *state.Repo, dir string, wantEnabled bool) ([]*state.Extension, error) {
	if dir != "" {
		e := r.FindExtension(dir)
		if e == nil {
			return nil, fmt.Errorf("no extension at %q (dirs: %s)", dir, strings.Join(repoDirs(r), ", "))
		}
		return []*state.Extension{e}, nil
	}
	var candidates []*state.Extension
	for i := range r.Extensions {
		if r.Extensions[i].Enabled() == wantEnabled {
			candidates = append(candidates, &r.Extensions[i])
		}
	}
	verb, prompt := "enable", "Enable which?"
	if wantEnabled {
		verb, prompt = "disable", "Disable which?"
	}
	switch {
	case len(candidates) == 0:
		return nil, fmt.Errorf("nothing to %s (see cepm list)", verb)
	case len(candidates) == 1:
		return candidates, nil
	case !assist.IsTTY():
		return nil, fmt.Errorf("multiple candidates; specify one: %s", strings.Join(candidateRefs(candidates), ", "))
	default:
		items := make([]string, len(candidates))
		for i, e := range candidates {
			items[i] = fmt.Sprintf("%-24s %s", e.Name, term.Safe(e.Dir))
		}
		picked, err := selectByNumbers(cmd, prompt, items)
		if err != nil {
			return nil, err
		}
		out := make([]*state.Extension, len(picked))
		for i, idx := range picked {
			out[i] = candidates[idx]
		}
		return out, nil
	}
}

func repoDirs(r *state.Repo) []string {
	dirs := make([]string, len(r.Extensions))
	for i, e := range r.Extensions {
		dirs[i] = e.Dir
	}
	return dirs
}

func candidateRefs(exts []*state.Extension) []string {
	refs := make([]string, len(exts))
	for i, e := range exts {
		refs[i] = e.Dir
	}
	return refs
}

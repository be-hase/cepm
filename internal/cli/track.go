package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/term"
	"github.com/be-hase/cepm/internal/updater"
)

func newTrackCmd() *cobra.Command {
	var (
		branch     string
		tagPattern string
		prerelease bool
		noReload   bool
	)
	cmd := &cobra.Command{
		Use:     "track <repo> <branch|tag>",
		Short:   "Switch how a repository follows its remote: a branch or release tags",
		GroupID: "ext",
		Long: `Switches a registered repository between branch and tag tracking without
reinstalling it. The clone moves to the new target (the latest matching tag,
or the branch tip), and extensions whose files changed are reloaded like an
update. Enable/disable choices and the clone itself are kept.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, mode := args[0], args[1]
			if mode != state.TrackBranch && mode != state.TrackTag {
				return fmt.Errorf(`track mode must be "branch" or "tag"`)
			}
			// Refused rather than ignored: a flag for the other mode is a
			// misunderstanding about what is being configured.
			if mode == state.TrackTag && branch != "" {
				return fmt.Errorf("--branch only applies to branch tracking")
			}
			if mode == state.TrackBranch && (tagPattern != "" || cmd.Flags().Changed("prerelease")) {
				return fmt.Errorf("--tag-pattern and --prerelease only apply to tag tracking")
			}
			res, err := updater.Retrack(cmd.Context(), name, updater.RetrackOptions{
				Track:         mode,
				Branch:        branch,
				TagPattern:    tagPattern,
				Prerelease:    prerelease,
				PrereleaseSet: cmd.Flags().Changed("prerelease"),
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			// Display only, hence Load without the lock: Retrack has finished,
			// and this reads back what it saved.
			if st, err := state.Load(); err == nil {
				if r, ok := st.Repos[name]; ok {
					if r.Track == state.TrackTag {
						fmt.Fprintf(out, "%q now tracks tags %q\n", name, r.TagPattern)
					} else {
						fmt.Fprintf(out, "%q now tracks branch %s\n", name, term.Safe(r.Branch))
					}
				}
			}
			return finishUpdate(cmd, []updater.RepoResult{res}, noReload)
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "branch to follow (branch mode; default: the recorded or remote default branch)")
	cmd.Flags().StringVar(&tagPattern, "tag-pattern", "", `glob for release tags in tag mode (default: the recorded pattern, then "v*")`)
	cmd.Flags().BoolVar(&prerelease, "prerelease", false, "in tag mode, also follow prerelease versions (v2.0.0-rc1)")
	cmd.Flags().BoolVar(&noReload, "no-reload", false, "pull only; do not reload extensions in Chrome")
	return cmd
}

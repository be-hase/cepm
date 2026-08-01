package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/paths"
	"github.com/be-hase/cepm/internal/term"
)

// newResetCmd moves the state and the clones out of the way so cepm can
// start over. It exists for the one situation cepm refuses to handle
// automatically: a state file it cannot use (corrupted, hand-edited, or from
// an incompatible build). Deleting the file alone would strand the clones —
// install refuses an occupied directory, and uninstall no longer knows the
// name — and would silently lose the repo URLs and Chrome bookkeeping, so
// everything is renamed into a backup instead. Nothing is deleted.
func newResetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reset",
		Short: "Move an unusable state and all clones to a backup and start over",
		Long: `Reset moves state.json and the cloned repositories into a time-stamped
backup directory under ~/.cepm. Use it when cepm reports that it cannot use
the state file. Nothing is deleted: the repo URLs can be read back from the
backed-up state.json, and the clones stay intact until you remove the backup
yourself.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			home, err := paths.CepmDir()
			if err != nil {
				return err
			}
			stateFile, err := paths.StateFile()
			if err != nil {
				return err
			}
			reposDir, err := paths.ReposDir()
			if err != nil {
				return err
			}

			backup := filepath.Join(home, "backup-"+time.Now().Format("20060102-150405"))
			moved := 0
			for _, src := range []string{stateFile, reposDir} {
				if _, err := os.Lstat(src); err != nil {
					continue // nothing there — half a reset is fine
				}
				if moved == 0 {
					if err := os.MkdirAll(backup, 0o755); err != nil {
						return err
					}
				}
				dst := filepath.Join(backup, filepath.Base(src))
				if err := os.Rename(src, dst); err != nil {
					return fmt.Errorf("move %s to the backup: %w", src, err)
				}
				fmt.Fprintf(out, "moved %s → %s\n", src, dst)
				moved++
			}
			if moved == 0 {
				fmt.Fprintln(out, "Nothing to reset: no state file and no clones.")
				return nil
			}
			fmt.Fprintf(out, "\nTo start over:\n")
			fmt.Fprintf(out, "  • the repository URLs are in %s\n", term.Quote(filepath.Join(backup, "state.json")))
			fmt.Fprintf(out, "  • re-run cepm install <url> for each repository you use\n")
			fmt.Fprintf(out, "  • entries may remain in chrome://extensions — remove the ones you no longer want\n")
			fmt.Fprintf(out, "  • once everything works, the backup can be deleted\n")
			return nil
		},
	}
}

package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/updater"
)

func newUninstallCmd() *cobra.Command {
	var keepFiles bool
	cmd := &cobra.Command{
		Use:     "uninstall <name>",
		Short:   "Unregister a repository and delete its clone",
		GroupID: "ext",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			var removed *state.Repo
			err := updater.WithLock(cmd.Context(), func() error {
				st, err := state.Load()
				if err != nil {
					return err
				}
				r, ok := st.Repos[name]
				if !ok {
					return fmt.Errorf("repository %q is not registered (see cepm list)", name)
				}
				removed = r
				delete(st.Repos, name)
				return st.Save()
			})
			if err != nil {
				return err
			}
			if !keepFiles {
				dir, err := updater.RepoDir(name)
				if err != nil {
					return err
				}
				if err := os.RemoveAll(dir); err != nil {
					return fmt.Errorf("unregistered, but failed to delete %s: %w", dir, err)
				}
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Uninstalled %q (%d extension(s)).\n", name, len(removed.Extensions))
			fmt.Fprintf(out, "Remove the extension(s) from chrome://extensions manually if they are still loaded.\n")
			return nil
		},
	}
	cmd.Flags().BoolVar(&keepFiles, "keep-files", false, "keep the cloned directory on disk")
	return cmd
}

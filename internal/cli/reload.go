package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/state"
)

func newReloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reload [name...]",
		Short: "Reload registered extensions in Chrome without pulling",
		Long: `Reload reloads extensions in Chrome without touching git. Useful after
editing files locally. With no arguments every registered extension is
reloaded; otherwise pass repository names.`,
		GroupID: "ext",
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := state.Load()
			if err != nil {
				return err
			}
			targets := args
			if len(targets) == 0 {
				targets = st.RepoNames()
			}
			var ids, names []string
			for _, name := range targets {
				r, ok := st.Repos[name]
				if !ok {
					return fmt.Errorf("repository %q is not registered (see cepm list)", name)
				}
				for _, e := range r.Extensions {
					ids = append(ids, e.ID)
					names = append(names, e.Name)
				}
			}
			if len(ids) == 0 {
				return fmt.Errorf("no extensions registered; run cepm install first")
			}
			reloadExtensions(cmd.Context(), cmd.OutOrStdout(), ids, names)
			return nil
		},
	}
}

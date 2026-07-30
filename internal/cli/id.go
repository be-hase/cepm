package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/extid"
)

func newIDCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "id <path>",
		Short: "Print the Chrome extension ID for an unpacked extension directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			abs, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			id, err := extid.FromPath(abs)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), id)
			return nil
		},
	}
}

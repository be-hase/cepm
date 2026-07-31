package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/extid"
	"github.com/be-hase/cepm/internal/scan"
)

func newIDCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "id <path>",
		Short: "Print the Chrome extension ID for an unpacked extension directory",
		Long: `Prints the ID Chrome assigns to the unpacked extension in <path>.

If its manifest.json pins the ID with a "key", that key decides the ID;
otherwise Chrome derives it from the absolute path.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			abs, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			key := ""
			switch m, err := scan.ReadManifest(abs); {
			case err == nil:
				key = m.Key
			case errors.Is(err, os.ErrNotExist):
				// No manifest: fall back to the path-derived ID, which is
				// what Chrome would use for a directory without a key.
			default:
				return err
			}
			id, err := extid.ForExtension(abs, key)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), id)
			return nil
		},
	}
}

package cli

import (
	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/nmhost"
)

func newNativeHostCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "native-host",
		Short:  "Native messaging host launched by Chrome (do not run directly)",
		Hidden: true,
		Args:   cobra.ArbitraryArgs, // Chrome passes the extension origin
		RunE: func(cmd *cobra.Command, args []string) error {
			return nmhost.Run(cmd.Context(), Version)
		},
	}
}

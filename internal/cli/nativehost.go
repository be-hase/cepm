package cli

import (
	"strings"

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
			for _, a := range args {
				if strings.HasPrefix(a, "chrome-extension://") {
					if err := nmhost.CheckOrigin(a); err != nil {
						return err
					}
				}
			}
			return nmhost.Run(cmd.Context(), Version)
		},
	}
}

package cli

import (
	"fmt"

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
			// Chrome always passes the caller's origin as the first
			// argument. Anything else — no argument, or a string that is
			// not the helper's exact origin — is not Chrome starting the
			// host, and the host must not come up for it.
			if len(args) == 0 {
				return fmt.Errorf("native-host is started by Chrome and expects the caller origin as its first argument")
			}
			if err := nmhost.CheckOrigin(args[0]); err != nil {
				return err
			}
			return nmhost.Run(cmd.Context(), Version)
		},
	}
}

// Package cli defines the cepm command tree.
package cli

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/launcher"
	"github.com/be-hase/cepm/internal/logx"
	"github.com/be-hase/cepm/internal/nmhost"
)

var verbose bool

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "cepm",
		Short: "Chrome Extension Package Manager for git-distributed unpacked extensions",
		Long: `cepm manages Chrome extensions distributed as git repositories.

It clones repositories, tracks branches or release tags, pulls updates
(automatically while Chrome is running, or via "cepm update"), and reloads
the affected unpacked extensions in Chrome through a small helper extension.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// Packages report their steps with slog.Debug; routing them to
			// stderr at debug level is what --verbose turns on.
			slog.SetDefault(logx.StderrLogger(verbose))
		},
	}
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false,
		"log what cepm does (git commands, host communication) to stderr")

	root.AddGroup(
		&cobra.Group{ID: "setup", Title: "Setup:"},
		&cobra.Group{ID: "ext", Title: "Extensions:"},
	)
	root.AddCommand(
		newSetupCmd(),
		newDoctorCmd(),
		newInstallCmd(),
		newUninstallCmd(),
		newEnableCmd(),
		newDisableCmd(),
		newUpdateCmd(),
		newReloadCmd(),
		newListCmd(),
		newCleanupCmd(),
		newIDCmd(),
		newVersionCmd(),
		newNativeHostCmd(),
	)
	return root
}

// Execute runs the cepm CLI.
func Execute() error {
	// Keep the host launcher pointing at whatever binary the user actually
	// runs, so upgrades that move the install path (mise, manual copies)
	// repair themselves on the next cepm invocation of any kind.
	launcher.SelfHeal()

	// The launcher execs "cepm native-host", but also handle Chrome invoking
	// this binary directly (a pre-launcher manifest): the extension origin
	// arrives as argv[1].
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "chrome-extension://") {
		if err := nmhost.CheckOrigin(os.Args[1]); err != nil {
			return err
		}
		return nmhost.Run(context.Background(), Version)
	}
	return newRootCmd().Execute()
}

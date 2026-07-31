// Package cli defines the cepm command tree.
package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/launcher"
	"github.com/be-hase/cepm/internal/logx"
	"github.com/be-hase/cepm/internal/nmhost"
	"github.com/be-hase/cepm/internal/state"
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
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Packages report their steps with slog.Debug; routing them to
			// stderr at debug level is what --verbose turns on.
			slog.SetDefault(logx.StderrLogger(verbose))
			return preflight(cmd)
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

// touchesChrome lists the commands that ask Chrome to change something. An
// inconsistent state must stop them *before* they act, not at the save that
// follows: earlier versions could write duplicate extension ids, so a file on
// disk may already be broken, and a removal cannot be undone.
var touchesChrome = map[string]bool{
	"install": true, "update": true, "reload": true,
	"enable": true, "disable": true, "cleanup": true,
}

// preflight refuses to run a Chrome-affecting command on a state cepm cannot
// reason about. "uninstall" is deliberately absent: removing a repository is
// how a duplicate gets resolved, and it skips its own Chrome-side step while
// the ambiguity lasts.
func preflight(cmd *cobra.Command) error {
	if !touchesChrome[cmd.Name()] {
		return nil
	}
	st, err := state.Load()
	if err != nil {
		return err
	}
	if err := st.Validate(); err != nil {
		return fmt.Errorf("%w\n(run cepm doctor for details)", err)
	}
	return nil
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

// Package cli defines the cepm command tree.
package cli

import (
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/launcher"
	"github.com/be-hase/cepm/internal/logx"
	"github.com/be-hase/cepm/internal/paths"
	"github.com/be-hase/cepm/internal/state"
	"github.com/be-hase/cepm/internal/term"
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

// touchesChrome lists the commands that ask Chrome or the filesystem to
// change something. An inconsistent state must stop them *before* they act,
// not at the save that follows: a removal cannot be undone.
var touchesChrome = map[string]bool{
	"install": true, "uninstall": true, "update": true, "reload": true,
	"enable": true, "disable": true, "cleanup": true,
}

// preflight refuses to run a state-changing command on a state cepm cannot
// reason about. No current cepm can save such a state (install and Save both
// refuse), so it is corruption or a hand edit — not something to repair by
// guessing, and fail-closed is the only safe answer.
func preflight(cmd *cobra.Command) error {
	if !touchesChrome[cmd.Name()] {
		return nil
	}
	st, err := state.Load()
	if err != nil {
		return err
	}
	if err := st.Validate(); err != nil {
		sf, pathErr := paths.StateFile()
		if pathErr != nil {
			sf = "~/.cepm/state.json"
		}
		return fmt.Errorf("%w\nNothing in Chrome or on disk was touched. cepm never writes such a state,"+
			"\nso it cannot repair it: delete %s and re-run cepm install"+
			"\nfor the repositories you use (cepm doctor has details)", err, term.Quote(sf))
	}
	return nil
}

// Execute runs the cepm CLI.
func Execute() error {
	// Keep the host launcher pointing at whatever binary the user actually
	// runs, so upgrades that move the install path (mise, manual copies)
	// repair themselves on the next cepm invocation of any kind.
	launcher.SelfHeal()

	return newRootCmd().Execute()
}

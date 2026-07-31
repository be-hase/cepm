package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/be-hase/cepm/internal/helperext"
	"github.com/be-hase/cepm/internal/ipc"
	"github.com/be-hase/cepm/internal/launcher"
	"github.com/be-hase/cepm/internal/nmmanifest"
	"github.com/be-hase/cepm/internal/paths"
)

func newSetupCmd() *cobra.Command {
	var (
		variant string
		force   bool
	)
	cmd := &cobra.Command{
		Use:     "setup",
		Short:   "Install the helper extension and native messaging host",
		GroupID: "setup",
		Args:    cobra.NoArgs,
		Long: `Setup prepares everything cepm needs inside Chrome:

  1. generates the cepm helper extension into ~/.cepm/helper
  2. registers this binary as a native messaging host

It is idempotent; re-run it after upgrading cepm, moving the binary, or
switching to a different Chrome (the previous Chrome's registration is
removed — cepm drives exactly one Chrome).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSetup(cmd, variant, force)
		},
	}
	cmd.Flags().StringVar(&variant, "chrome-variant", "stable",
		fmt.Sprintf("the Chrome you use, one of %v", paths.ChromeVariants))
	cmd.Flags().BoolVar(&force, "force", false, "regenerate the helper extension even if up to date")
	return cmd
}

func runSetup(cmd *cobra.Command, variant string, force bool) error {
	out := cmd.OutOrStdout()
	if err := paths.EnsureLayout(); err != nil {
		return err
	}
	helperDir, err := paths.HelperDir()
	if err != nil {
		return err
	}

	installedVersion := helperext.InstalledVersion(helperDir)
	helperChanged := force || installedVersion != helperext.Version
	if helperChanged {
		if err := helperext.Install(helperDir); err != nil {
			return fmt.Errorf("install helper extension: %w", err)
		}
		fmt.Fprintf(out, "✔ Helper extension written to %s (v%s)\n", helperDir, helperext.Version)
	} else {
		fmt.Fprintf(out, "✔ Helper extension up to date at %s (v%s)\n", helperDir, helperext.Version)
	}

	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve cepm binary path: %w", err)
	}
	if err := launcher.Install(binPath); err != nil {
		return fmt.Errorf("install host launcher: %w", err)
	}
	launcherPath, err := paths.LauncherPath()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "✔ Host launcher written to %s (binary: %s)\n", launcherPath, binPath)
	path, err := nmmanifest.Install(variant, launcherPath)
	if err != nil {
		return fmt.Errorf("install native messaging manifest (%s): %w", variant, err)
	}
	fmt.Fprintf(out, "✔ Native messaging manifest written to %s\n", path)
	// One Chrome at a time: a manifest left in another variant would let a
	// second Chrome connect, and only one of them would receive reloads.
	removed, err := nmmanifest.RemoveOthers(variant)
	if err != nil {
		return fmt.Errorf("remove old native messaging manifests: %w", err)
	}
	for _, p := range removed {
		fmt.Fprintf(out, "✔ Removed registration for a previously used Chrome: %s\n", p)
	}
	if len(removed) > 0 {
		// Removing a manifest does not touch a host the old Chrome already
		// started — and as long as that Chrome runs the helper, it restarts
		// the host (and can hold the leader role) no matter what we do here.
		// The switch is only complete once the old Chrome exits.
		pingCtx, pingCancel := context.WithTimeout(cmd.Context(), 2*time.Second)
		_, pingErr := ipc.Ping(pingCtx)
		pingCancel()
		if pingErr == nil {
			fmt.Fprintf(out, "\n⚠ A cepm host is still running — likely started by the previous Chrome.\n")
			fmt.Fprintf(out, "  Quit that Chrome completely; until then it may keep receiving the updates.\n")
		}
	}

	// A running helper keeps its current code until Chrome restarts: the only
	// self-reload API (chrome.runtime.reload) drops the native messaging port
	// and Chrome may not restart the helper's service worker afterwards.
	if helperChanged && installedVersion != "" {
		ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
		defer cancel()
		if _, err := ipc.Ping(ctx); err == nil {
			fmt.Fprintln(out, "  (the running helper keeps its current version until Chrome restarts)")
		}
	}

	fmt.Fprintf(out, `
One-time step (skip if already done):
  1. Open chrome://extensions
  2. Turn on "Developer mode" (top right)
  3. Click "Load unpacked" and select: %s
     (extension ID will be %s)

Then verify with: cepm doctor
`, helperDir, helperext.ExtensionID())
	return nil
}

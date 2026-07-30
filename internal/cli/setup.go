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
		variants []string
		force    bool
	)
	cmd := &cobra.Command{
		Use:     "setup",
		Short:   "Install the helper extension and native messaging host",
		GroupID: "setup",
		Args:    cobra.NoArgs,
		Long: `Setup prepares everything cepm needs inside Chrome:

  1. generates the cepm helper extension into ~/.cepm/helper
  2. registers this binary as a native messaging host

It is idempotent; re-run it after upgrading cepm or moving the binary.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSetup(cmd, variants, force)
		},
	}
	cmd.Flags().StringSliceVar(&variants, "chrome-variant", []string{"stable"},
		fmt.Sprintf("Chrome variants to configure %v", paths.ChromeVariants))
	cmd.Flags().BoolVar(&force, "force", false, "regenerate the helper extension even if up to date")
	return cmd
}

func runSetup(cmd *cobra.Command, variants []string, force bool) error {
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
	for _, v := range variants {
		path, err := nmmanifest.Install(v, launcherPath)
		if err != nil {
			return fmt.Errorf("install native messaging manifest (%s): %w", v, err)
		}
		fmt.Fprintf(out, "✔ Native messaging manifest written to %s\n", path)
	}

	// If a live helper is already connected, push the new helper files into
	// Chrome by asking it to reload itself.
	if helperChanged && installedVersion != "" {
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
		defer cancel()
		if _, err := ipc.Reload(ctx, []string{helperext.ExtensionID()}); err == nil {
			fmt.Fprintln(out, "✔ Running helper extension reloaded")
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

package commands

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/semanticash/cli/internal/launcher"
)

// NewLauncherCmd returns the macOS launcher management command.
func NewLauncherCmd(rootOpts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "launcher",
		Short: "Manage the optional macOS launchd worker agent (experimental)",
		Long: `Opt in or out of the macOS launchd-based post-commit worker.

Enabling the launcher installs a LaunchAgent plist under
~/Library/LaunchAgents and records the choice in
~/.semantica/settings.json. The agent runs a short-lived worker
drain on demand whenever the post-commit hook kicks it.

Disabling removes the plist, unloads the agent, and clears the
settings flag. Commits fall back to the legacy detached-spawn
path, which is the same path users who never opted in have
always used.

The launcher is macOS-only and currently experimental. Consider
it a follow-up to semantica enable rather than a replacement.`,
	}
	cmd.AddCommand(newLauncherEnableCmd(rootOpts))
	cmd.AddCommand(newLauncherDisableCmd(rootOpts))
	return cmd
}

func newLauncherEnableCmd(rootOpts *RootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "enable",
		Short: "Install and bootstrap the launchd worker agent",
		Long: `Install the LaunchAgent plist for the semantica worker and
bootstrap it into the current user's launchd domain. Safe to
run on an already-enabled system: the plist is re-rendered
against the current binary path and the agent is re-bootstrapped
so the system lands in a known-good state.

Produces a background item notification on macOS Ventura and
later. Run semantica launcher disable to undo.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("launcher enable: resolve semantica binary: %w", err)
			}
			result, err := launcher.Enable(cmd.Context(), exe)
			if err != nil {
				return err
			}
			printEnableResult(cmd.OutOrStdout(), result)
			return nil
		},
	}
}

func newLauncherDisableCmd(rootOpts *RootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "disable",
		Short: "Unload and remove the launchd worker agent",
		Long: `Unload the LaunchAgent, remove the plist file, and clear the
launcher flag in user settings. Idempotent: running disable on
an already-disabled system is a silent no-op. Commits revert to
the legacy detached-spawn path.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := launcher.Disable(cmd.Context())
			if err != nil {
				return err
			}
			printDisableResult(cmd.OutOrStdout(), result)
			return nil
		},
	}
}

func printEnableResult(w io.Writer, r *launcher.InstallResult) {
	verb := "Installed"
	if r.Reinstalled {
		verb = "Re-installed"
	}
	fmt.Fprintf(w, "%s launch agent: %s\n", verb, r.PlistPath)
	fmt.Fprintf(w, "Bootstrapped:        %s\n", r.DomainTarget)
	fmt.Fprintln(w, "Run 'semantica launcher disable' to undo.")
}

func printDisableResult(w io.Writer, r *launcher.DisableResult) {
	if !r.WasEnabled && r.RemovedPlistPath == "" {
		fmt.Fprintln(w, "Launcher was not installed; nothing to do.")
		return
	}
	if r.RemovedPlistPath != "" {
		fmt.Fprintf(w, "Removed launch agent: %s\n", r.RemovedPlistPath)
	}
	fmt.Fprintln(w, "Launchd agent unloaded. Commits now use the legacy spawn path.")
}

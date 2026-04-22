package commands

import (
	"fmt"
	"io"
	"os"
	"time"

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
	cmd.AddCommand(newLauncherStatusCmd(rootOpts))
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

func newLauncherStatusCmd(rootOpts *RootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the launcher's current state",
		Long: `Print the launcher's current state, drawing from three independent
sources at once:

- the user-level settings file at ~/.semantica/settings.json,
- the plist file on disk under ~/Library/LaunchAgents,
- launchd itself (via launchctl print).

Presenting all three together makes drift visible: a settings
file that claims enabled while launchd does not, a plist file
that exists but was never bootstrapped, and so on. Useful as the
first place to look when troubleshooting.`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			status, err := launcher.Status(cmd.Context())
			if err != nil {
				return err
			}
			printLauncherStatus(cmd.OutOrStdout(), status)
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

// printLauncherStatus renders launcher status as a human-readable block.
func printLauncherStatus(w io.Writer, s launcher.StatusResult) {
	enabledText := "not enabled"
	if s.SettingsEnabled {
		enabledText = "enabled"
	}
	fmt.Fprintf(w, "Launcher:          %s\n", enabledText)

	// Keep settings errors near the top.
	if s.SettingsError != "" {
		fmt.Fprintf(w, "Settings error:    %s\n", s.SettingsError)
	}

	if s.InstalledAt > 0 {
		t := time.UnixMilli(s.InstalledAt).Local().Format(time.RFC3339)
		fmt.Fprintf(w, "Installed at:      %s\n", t)
	}

	plistPath := s.InstalledPlistPath
	if plistPath == "" {
		plistPath = s.ExpectedPlistPath
	}
	fmt.Fprintf(w, "Plist path:        %s\n", plistPath)

	plistState := "missing"
	if s.PlistOnDisk {
		plistState = "present"
	}
	fmt.Fprintf(w, "Plist on disk:     %s\n", plistState)

	fmt.Fprintf(w, "Domain target:     %s\n", s.DomainTarget)
	fmt.Fprintf(w, "Launchd state:     %s\n", s.LaunchdState)
	fmt.Fprintf(w, "Log path:          %s\n", s.LogPath)

	// Show a single actionable hint when one applies. Unsupported hosts
	// take precedence because launcher commands are macOS-only.
	switch {
	case s.LaunchdState == "unsupported":
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "The launcher is only available on macOS.")
	case s.SettingsError != "":
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "Fix or remove the settings file and rerun 'semantica launcher enable'.")
	case !s.SettingsEnabled && s.LoadedInLaunchd:
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "Drift: launchd has the service loaded, but settings say not enabled.")
		fmt.Fprintln(w, "Run 'semantica launcher disable' to clean up.")
	case s.SettingsEnabled && !s.LoadedInLaunchd && s.LaunchdState == "not loaded":
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "Drift: settings say enabled, but launchd has no loaded service.")
		fmt.Fprintln(w, "Run 'semantica launcher enable' to reinstall cleanly.")
	case s.SettingsEnabled && !s.PlistOnDisk:
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "Drift: settings say enabled, but the plist file is missing.")
		fmt.Fprintln(w, "Run 'semantica launcher enable' to reinstall cleanly.")
	case !s.SettingsEnabled:
		fmt.Fprintln(w, "")
		fmt.Fprintln(w, "Run 'semantica launcher enable' to opt in.")
	}
}

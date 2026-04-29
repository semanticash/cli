package commands

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/semanticash/cli/internal/launcher"
)

// NewLauncherCmd returns the launcher management command.
func NewLauncherCmd(rootOpts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "launcher",
		Short: "Manage the optional background worker agent (experimental)",
		Long: `Opt in or out of the OS-managed post-commit worker.

Enabling the launcher installs a service definition with the OS
daemon manager (launchd on macOS, systemd user units on Linux,
Task Scheduler on Windows) and records the choice in
~/.semantica/settings.json. The service runs a short-lived worker
drain on demand whenever the post-commit hook kicks it.

Disabling unregisters the service, removes its definition file,
and clears the settings flag. Commits fall back to the legacy
detached-spawn path, which is the same path users who never
opted in have always used.

The launcher is currently experimental and supported on macOS,
Linux (systemd user instance required), and Windows (Task
Scheduler). Other platforms fall back to the legacy spawn path.
Consider 'launcher enable' a follow-up to 'semantica enable'
rather than a replacement.`,
	}
	cmd.AddCommand(newLauncherEnableCmd(rootOpts))
	cmd.AddCommand(newLauncherDisableCmd(rootOpts))
	cmd.AddCommand(newLauncherStatusCmd(rootOpts))
	return cmd
}

func newLauncherEnableCmd(rootOpts *RootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "enable",
		Short: "Install and register the worker service",
		Long: `Install the worker service definition (LaunchAgent plist on
macOS, systemd user unit on Linux, Task Scheduler task on
Windows) and register it with the OS daemon manager. Safe to
run on an already-enabled system: the definition is re-rendered
against the current binary path and re-registered so the system
lands in a known-good state.

Produces a background item notification on macOS Ventura and
later. Run 'semantica launcher disable' to undo.`,
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
		Short: "Unregister and remove the worker service",
		Long: `Unregister the worker service from the OS daemon manager,
remove its definition file, and clear the launcher flag in user
settings. Idempotent: running disable on an already-disabled
system is a silent no-op. Commits revert to the legacy
detached-spawn path.

Disable is best-effort: missing files, missing registrations,
and unreachable daemon managers (degraded systemd user instance,
broken dbus, stopped Task Scheduler service, etc.) are tolerated
so cleanup still completes in recovery scenarios.`,
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
- the unit/plist/task definition file on disk,
- the OS daemon manager itself (launchctl on macOS, systemctl
  --user on Linux, schtasks on Windows).

Presenting all three together makes drift visible: a settings
file that claims enabled while the daemon manager does not, a
definition file that exists but was never registered, and so on.
Useful as the first place to look when troubleshooting.`,
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
	_, _ = fmt.Fprintf(w, "%s service: %s\n", verb, r.UnitPath)
	_, _ = fmt.Fprintf(w, "Registered:        %s\n", r.UnitTarget)
	_, _ = fmt.Fprintln(w, "Run 'semantica launcher disable' to undo.")
}

func printDisableResult(w io.Writer, r *launcher.DisableResult) {
	if !r.WasEnabled && r.RemovedUnitPath == "" {
		_, _ = fmt.Fprintln(w, "Launcher was not installed; nothing to do.")
		return
	}
	if r.RemovedUnitPath != "" {
		_, _ = fmt.Fprintf(w, "Removed service: %s\n", r.RemovedUnitPath)
	}
	_, _ = fmt.Fprintln(w, "Service unregistered. Commits now use the legacy spawn path.")
}

// printLauncherStatus renders launcher status as a human-readable
// block.
func printLauncherStatus(w io.Writer, s launcher.StatusResult) {
	enabledText := "not enabled"
	if s.SettingsEnabled {
		enabledText = "enabled"
	}
	_, _ = fmt.Fprintf(w, "Launcher:          %s\n", enabledText)

	// Keep settings errors near the top.
	if s.SettingsError != "" {
		_, _ = fmt.Fprintf(w, "Settings error:    %s\n", s.SettingsError)
	}

	if s.InstalledAt > 0 {
		t := time.UnixMilli(s.InstalledAt).Local().Format(time.RFC3339)
		_, _ = fmt.Fprintf(w, "Installed at:      %s\n", t)
	}

	unitPath := s.InstalledUnitPath
	if unitPath == "" {
		unitPath = s.ExpectedUnitPath
	}
	_, _ = fmt.Fprintf(w, "Unit path:         %s\n", unitPath)

	unitState := "missing"
	if s.UnitOnDisk {
		unitState = "present"
	}
	_, _ = fmt.Fprintf(w, "Unit on disk:      %s\n", unitState)

	_, _ = fmt.Fprintf(w, "Service target:    %s\n", s.UnitTarget)
	_, _ = fmt.Fprintf(w, "Service state:     %s\n", s.ServiceState)
	_, _ = fmt.Fprintf(w, "Log path:          %s\n", s.LogPath)

	// Show a single actionable hint when one applies. Daemon-
	// manager states that prevent the launcher from operating
	// (no backend at all, or a backend present but failing) take
	// precedence: no other hint can resolve those, and the
	// fallback `Run 'semantica launcher enable' to opt in` would
	// be actively wrong on a host where enable cannot succeed.
	switch {
	case s.ServiceState == "unsupported", strings.HasPrefix(s.ServiceState, "error:"):
		_, _ = fmt.Fprintln(w, "")
		_, _ = fmt.Fprintln(w, unsupportedHint(s.OS))
	case s.SettingsError != "":
		_, _ = fmt.Fprintln(w, "")
		_, _ = fmt.Fprintln(w, "Fix or remove the settings file and rerun 'semantica launcher enable'.")
	case !s.SettingsEnabled && s.LoadedInDaemon:
		_, _ = fmt.Fprintln(w, "")
		_, _ = fmt.Fprintln(w, "Drift: the OS daemon manager has the service loaded, but settings say not enabled.")
		_, _ = fmt.Fprintln(w, "Run 'semantica launcher disable' to clean up.")
	case s.SettingsEnabled && !s.LoadedInDaemon && s.ServiceState == "not loaded":
		_, _ = fmt.Fprintln(w, "")
		_, _ = fmt.Fprintln(w, "Drift: settings say enabled, but the OS daemon manager has no loaded service.")
		_, _ = fmt.Fprintln(w, "Run 'semantica launcher enable' to reinstall cleanly.")
	case s.SettingsEnabled && !s.UnitOnDisk:
		_, _ = fmt.Fprintln(w, "")
		_, _ = fmt.Fprintln(w, "Drift: settings say enabled, but the unit file is missing.")
		_, _ = fmt.Fprintln(w, "Run 'semantica launcher enable' to reinstall cleanly.")
	case !s.SettingsEnabled:
		_, _ = fmt.Fprintln(w, "")
		_, _ = fmt.Fprintln(w, "Run 'semantica launcher enable' to opt in.")
	}
}

// unsupportedHint returns a host-specific message for the
// "ServiceState=unsupported" branch of printLauncherStatus.
//
// On hosts where the launcher backend exists at compile time
// (darwin, linux) but the daemon manager is unreachable at
// runtime, the hint describes the runtime issue rather than
// claiming the OS lacks support entirely. On other hosts
// (for example BSDs), the hint reflects that no
// backend exists for that OS.
func unsupportedHint(goos string) string {
	switch goos {
	case "linux":
		return "The systemd user instance is not reachable. Ensure XDG_RUNTIME_DIR is set and `systemctl --user` works on this host."
	case "darwin":
		return "launchd is not reachable on this host."
	case "windows":
		return "Task Scheduler is not reachable. Ensure the Task Scheduler service is running and `schtasks /Query /TN \\Semantica\\sh.semantica.worker` works on this host."
	default:
		return "The launcher has no backend on this OS. Supported: macOS (launchd), Linux (systemd user instance), and Windows (Task Scheduler)."
	}
}

package commands

import (
	"fmt"
	"os"
	"time"

	"charm.land/huh/v2"
	"github.com/semanticash/cli/internal/service/handoff"
	"github.com/spf13/cobra"
)

// isInteractiveCmd reports whether a cobra.Command's wired stdio
// is a real TTY on both ends. The handoff flow uses it to gate
// blocking UI (the [Y/n] confirm and the provider picker) so
// piped or scripted callers never hang waiting for input.
//
// Both gates matter: stdout-only TTY would show a prompt the user
// can see but never answer; stdin-only TTY would block on
// ReadString against a sink that consumes no output.
//
// Indirected through isInteractiveCmdFn so tests can flip the
// gate without owning a real pty. The picker logic itself
// (which the tests want to drive) cannot be exercised through a
// pipe-based fake.
var isInteractiveCmdFn = defaultIsInteractiveCmd

func isInteractiveCmd(cmd *cobra.Command) bool {
	return isInteractiveCmdFn(cmd)
}

func defaultIsInteractiveCmd(cmd *cobra.Command) bool {
	if !isTerminalWriterFn(cmd.OutOrStdout()) {
		return false
	}
	inFile, ok := cmd.InOrStdin().(*os.File)
	if !ok || !isInteractiveTerminal(inFile) {
		return false
	}
	return true
}

// pickActiveProviderFn is the package-level seam for the picker.
// Production uses huhPickActiveProvider, which renders a
// charm.land/huh select form. Tests stub this to a deterministic
// picker so the ambiguity-resolution flow can be exercised
// end-to-end without a real TTY.
var pickActiveProviderFn = huhPickActiveProvider

// pickActiveProvider shows the user a list of distinct active
// providers and returns the hook-form name they chose. The list
// is the ActiveProvider slice the service layer returned with the
// AmbiguousActiveSessionError; the picker label includes the
// count of active capture states and a relative-time hint so the
// user can spot stale clusters.
func pickActiveProvider(cmd *cobra.Command, providers []handoff.ActiveProvider) (string, error) {
	return pickActiveProviderFn(cmd, providers)
}

func huhPickActiveProvider(_ *cobra.Command, providers []handoff.ActiveProvider) (string, error) {
	options := make([]huh.Option[string], len(providers))
	for i, p := range providers {
		label := fmt.Sprintf("%-12s (%d active, latest %s ago)",
			p.Provider, p.Count, humanizeAgo(time.Since(p.LatestTimestamp)))
		options[i] = huh.NewOption(label, p.Provider)
	}
	var selected string
	form := huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("Multiple active providers - pick the handoff source").
			Options(options...).
			Value(&selected),
	))
	if err := form.Run(); err != nil {
		return "", fmt.Errorf("provider selection: %w", err)
	}
	return selected, nil
}

// humanizeAgo renders a duration as a compact label for the
// picker. Tuned for short durations (active states are bounded by
// the 24h recency window upstream); past 24h shows hours rather
// than rolling over to days so the user always sees a number.
func humanizeAgo(d time.Duration) string {
	switch {
	case d < 30*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

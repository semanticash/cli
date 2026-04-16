package hooks

import (
	"fmt"
	"strings"
)

// ManagedCommand is the CLI command provider hooks should invoke by default.
// It intentionally uses the bare executable name so hook configs survive moves
// between install locations like Homebrew, curl installs, and local rebuilds.
const ManagedCommand = "semantica"

// GuardedCommand wraps a capture command with a shell guard that silently
// no-ops when the binary is not on PATH. Ensures the hook never blocks the
// agent and never produces errors for teammates who don't have Semantica.
func GuardedCommand(bin, args string) string {
	cmd := bin + " " + args
	return fmt.Sprintf("if command -v %s >/dev/null 2>&1; then %s || true; fi", bin, cmd)
}

// ExtractBinary returns the binary path from a hook command string.
// Handles both guarded ("if command -v X ...; then X capture ...; fi")
// and unguarded ("X capture ...") formats.
func ExtractBinary(command string) string {
	// Guarded format: extract binary after "then "
	if idx := strings.Index(command, "; then "); idx != -1 {
		after := command[idx+7:] // skip "; then "
		parts := strings.Fields(after)
		if len(parts) > 0 {
			return parts[0]
		}
	}
	// Unguarded format: first token
	parts := strings.Fields(command)
	if len(parts) > 0 {
		return parts[0]
	}
	return ""
}

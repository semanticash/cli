//go:build !windows

package commands

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/semanticash/cli/internal/service/handoff"
)

// defaultExecutor on Unix replaces the running semantica process
// with the agent binary using syscall.Exec, so the user lands
// directly in the agent's interactive shell with no parent
// process hanging around. Returns only on error; a successful
// exec never returns.
func defaultExecutor(workdir string, spec *handoff.LaunchSpec) error {
	bin, err := exec.LookPath(spec.Binary)
	if err != nil {
		return fmt.Errorf("agent binary %q not found on PATH", spec.Binary)
	}
	if err := os.Chdir(workdir); err != nil {
		return fmt.Errorf("chdir %s: %w", workdir, err)
	}
	argv := append([]string{spec.Binary}, spec.Args...)
	if err := syscall.Exec(bin, argv, os.Environ()); err != nil {
		return fmt.Errorf("exec %s: %w", spec.Binary, err)
	}
	// Unreachable on a successful exec.
	return nil
}

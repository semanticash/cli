//go:build windows

package commands

import (
	"fmt"

	"github.com/semanticash/cli/internal/service/handoff"
)

// defaultExecutor on Windows currently has no process-replacement
// equivalent. Rather than spawn a child and have the user end up
// inside a wrapped subshell, surface a clear "not yet supported"
// message and let them run the same command manually. The print
// path in BuildLaunchSpec(..., printOnly=true) is the right
// pattern; this is just the runtime guard if exec was somehow
// reached on Windows.
func defaultExecutor(workdir string, spec *handoff.LaunchSpec) error {
	return fmt.Errorf("auto-launch not supported on Windows yet; rerun with --print and execute the command manually")
}

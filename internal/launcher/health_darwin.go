//go:build darwin

package launcher

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
)

// daemonServiceHealth captures `launchctl print` output for the worker
// target and extracts spawn-health signals. Best-effort: failures return
// zero health, and Status reports registration separately.
func daemonServiceHealth(ctx context.Context) ServiceHealth {
	cmd := exec.CommandContext(ctx, "launchctl", "print", UnitTarget())
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	_ = cmd.Run()
	return parseServiceHealth(out.String())
}

// parseServiceHealth extracts spawn-health signals from launchctl print
// output. The important darwin case is a registered worker that launchd
// refuses to spawn because its binary identity changed.
func parseServiceHealth(printOutput string) ServiceHealth {
	var h ServiceHealth
	for _, line := range strings.Split(printOutput, "\n") {
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, "last exit reason"); ok {
			h.LastExitReason = strings.TrimSpace(strings.TrimLeft(rest, " =:"))
			continue
		}
		if strings.Contains(trimmed, "needs LWCR update") {
			h.NeedsLWCRUpdate = true
		}
	}
	h.SpawnRefused = h.NeedsLWCRUpdate || strings.Contains(h.LastExitReason, "CODESIGNING")
	return h
}

//go:build darwin

package launcher

import "testing"

// parseServiceHealth must recognize the stale-binary refusal shape
// observed in the field: launchd accepts kickstarts but refuses spawns
// after the registered binary is replaced in place.
func TestParseServiceHealth(t *testing.T) {
	refusing := `sh.semantica.worker = {
	active count = 0
	path = /Users/u/Library/LaunchAgents/sh.semantica.worker.plist
	state = not running
	program = /usr/local/bin/semantica
	runs = 12
	last exit reason = OS_REASON_CODESIGNING
	properties = inferred program | needs LWCR update
}`
	h := parseServiceHealth(refusing)
	if h.LastExitReason != "OS_REASON_CODESIGNING" {
		t.Errorf("last exit reason = %q", h.LastExitReason)
	}
	if !h.NeedsLWCRUpdate {
		t.Error("needs LWCR update not detected")
	}
	if !h.SpawnRefused {
		t.Error("refusal state not summarized as SpawnRefused")
	}

	healthy := `sh.semantica.worker = {
	state = not running
	runs = 13
	last exit code = 0
	properties = inferred program | managed LWCR | has LWCR
}`
	h = parseServiceHealth(healthy)
	if h.SpawnRefused {
		t.Errorf("healthy service reported refusing: %+v", h)
	}
	if h.NeedsLWCRUpdate {
		t.Error("managed LWCR must not read as needs-update")
	}

	// Empty output (service not loaded / print failed): zero health.
	if h := parseServiceHealth(""); h.SpawnRefused || h.LastExitReason != "" {
		t.Errorf("empty output must yield zero health: %+v", h)
	}
}

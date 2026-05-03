package hooks

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMarshalSettingsJSON_DoesNotEscapeShellMetacharacters(t *testing.T) {
	cmd := GuardedCommand("semantica", "capture gemini-cli after-tool")

	got, err := MarshalSettingsJSON(map[string]string{"command": cmd})
	if err != nil {
		t.Fatalf("MarshalSettingsJSON: %v", err)
	}
	out := string(got)

	if strings.Contains(out, `\u003e`) || strings.Contains(out, `\u0026`) {
		t.Errorf("expected unescaped shell chars, got escaped form: %s", out)
	}
	if !strings.Contains(out, ">/dev/null") || !strings.Contains(out, "2>&1") {
		t.Errorf("expected literal `>/dev/null` and `2>&1` in output, got: %s", out)
	}
	if strings.HasSuffix(out, "\n") {
		t.Errorf("expected no trailing newline, got %q", out)
	}
}

func TestMarshalSettingsJSON_RoundTripsToSameString(t *testing.T) {
	cmd := GuardedCommand("semantica", "capture claude-code stop")

	got, err := MarshalSettingsJSON(map[string]string{"command": cmd})
	if err != nil {
		t.Fatalf("MarshalSettingsJSON: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("re-parse failed: %v", err)
	}
	if parsed["command"] != cmd {
		t.Errorf("round-trip mismatch:\n got %q\nwant %q", parsed["command"], cmd)
	}
}

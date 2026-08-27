package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestCrossRepoFixture_ObservedShape pins a sanitized Codex Bash payload whose
// tool input has no command-specific working directory. Provider upgrades
// require a new runtime probe; this static fixture cannot detect payload changes.
func TestCrossRepoFixture_ObservedShape(t *testing.T) {
	p := &Provider{}
	for _, tc := range []struct{ name, capture, file string }{
		{"pre", "pre-tool-use", "testdata/crossrepo/pre-tool-use.json"},
		{"post", "post-tool-use", "testdata/crossrepo/post-tool-use.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Clean(tc.file))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}

			event, err := p.ParseHookEvent(context.Background(), tc.capture, bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if event == nil {
				t.Fatal("parse returned nil")
			}

			// CWD identifies the session directory.
			const sessionCWD = "/Users/dev/projects/cli"
			if event.CWD != sessionCWD {
				t.Errorf("Event.CWD = %q, want the session directory %q", event.CWD, sessionCWD)
			}

			// The observed tool input contains only command.
			var toolInput map[string]json.RawMessage
			if err := json.Unmarshal(event.ToolInput, &toolInput); err != nil {
				t.Fatalf("decode tool_input: %v", err)
			}
			if len(toolInput) != 1 {
				t.Errorf("tool_input keys = %v, want exactly {command}", keysOf(toolInput))
			}
			if _, ok := toolInput["command"]; !ok {
				t.Errorf("tool_input keys = %v, want exactly {command}", keysOf(toolInput))
			}
		})
	}
}

package cursor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// Relative command directories resolve against the session directory.
func TestCursor_ShellRelativeCommandCWD(t *testing.T) {
	p := &Provider{}
	sessionCWD := filepath.Join(t.TempDir(), "repoA")
	rel := `{"conversation_id":"c","cwd":` + jsonQuote(sessionCWD) + `,"tool_name":"Bash","tool_use_id":"call 9","tool_input":{"command":"x","cwd":"nested/dir"}}`
	ev, err := p.ParseHookEvent(context.Background(), "pre-tool-use", strings.NewReader(rel))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if want := filepath.Join(sessionCWD, "nested", "dir"); ev.EffectiveCWD != want {
		t.Errorf("EffectiveCWD = %q, want %q (relative resolved against session cwd)", ev.EffectiveCWD, want)
	}

	// A relative session directory cannot anchor another relative path.
	noBase := `{"conversation_id":"c","cwd":"relative-session","tool_name":"Bash","tool_use_id":"call 9","tool_input":{"command":"x","cwd":"nested/dir"}}`
	ev2, err := p.ParseHookEvent(context.Background(), "pre-tool-use", strings.NewReader(noBase))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev2.EffectiveCWD != "" {
		t.Errorf("EffectiveCWD = %q, want empty when no absolute base exists", ev2.EffectiveCWD)
	}
}

// Cursor exposes the shell command directory on pre- and post-tool events.
func TestCursor_ShellEffectiveCWD(t *testing.T) {
	p := &Provider{}
	base := t.TempDir()
	sessionCWD := filepath.Join(base, "repoA")
	commandCWD := filepath.Join(base, "repoB")
	payload := `{"conversation_id":"conv-1","cwd":` + jsonQuote(sessionCWD) + `,"tool_name":"Bash","tool_use_id":"call 9","tool_input":{"command":"sqlc generate","cwd":` + jsonQuote(commandCWD) + `}}`

	for _, hook := range []string{"pre-tool-use", "post-tool-use"} {
		t.Run(hook, func(t *testing.T) {
			ev, err := p.ParseHookEvent(context.Background(), hook, strings.NewReader(payload))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if ev == nil {
				t.Fatal("nil event")
			}
			if ev.CWD != sessionCWD {
				t.Errorf("CWD = %q, want the session dir %q", ev.CWD, sessionCWD)
			}
			if ev.EffectiveCWD != commandCWD {
				t.Errorf("EffectiveCWD = %q, want the command dir %q", ev.EffectiveCWD, commandCWD)
			}
		})
	}
}

// A missing command directory leaves session routing unchanged.
func TestCursor_ShellNoCommandCWD(t *testing.T) {
	p := &Provider{}
	payload := `{"conversation_id":"conv-1","cwd":"/work/repoA","tool_name":"Bash","tool_use_id":"call 9","tool_input":{"command":"echo hi"}}`
	ev, err := p.ParseHookEvent(context.Background(), "pre-tool-use", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ev.EffectiveCWD != "" {
		t.Errorf("EffectiveCWD = %q, want empty when tool_input has no cwd", ev.EffectiveCWD)
	}
	if ev.CWD != "/work/repoA" {
		t.Errorf("CWD = %q, want /work/repoA", ev.CWD)
	}
}

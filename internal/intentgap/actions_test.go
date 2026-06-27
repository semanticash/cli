package intentgap

import (
	"strings"
	"testing"
)

// Claude Edit produces one action whose file_path comes from
// tool_uses. The Sources field records that provenance.
func TestExtractActions_ClaudeEditFromToolUses(t *testing.T) {
	row := ActionEventRow{
		TurnID:       "t-1",
		CheckpointID: "ck-1",
		TS:           1000,
		ToolUses:     `{"tools":[{"name":"Edit","file_path":"internal/handler.go"}]}`,
	}
	got := ExtractActions(row, nil, "")
	if len(got) != 1 {
		t.Fatalf("expected 1 action, got %d", len(got))
	}
	a := got[0]
	if a.ToolName != "Edit" || a.FilePath != "internal/handler.go" {
		t.Errorf("tool=%q file=%q, want Edit / internal/handler.go", a.ToolName, a.FilePath)
	}
	if !hasSource(a, "file_path:tool_uses") {
		t.Errorf("missing file_path:tool_uses source: %v", a.Sources)
	}
}

// Provider-native file-edit events (Cursor here) produce actions
// the same way as Claude Edit/Write. The tool name is preserved.
func TestExtractActions_ProviderFileEditFromToolUses(t *testing.T) {
	row := ActionEventRow{
		TurnID:   "t-1",
		ToolUses: `{"tools":[{"name":"cursor_file_edit","file_path":"main.go"}]}`,
	}
	got := ExtractActions(row, nil, "")
	if len(got) != 1 || got[0].ToolName != "cursor_file_edit" || got[0].FilePath != "main.go" {
		t.Errorf("got %+v, want one cursor_file_edit on main.go", got)
	}
}

// Bash with an `rm` command produces one action per concrete target.
func TestExtractActions_BashRMDerivesPathsFromPayload(t *testing.T) {
	row := ActionEventRow{
		TurnID:       "t-1",
		CheckpointID: "ck-1",
		ToolUses:     `{"tools":[{"name":"Bash","file_op":"exec"}]}`,
	}
	payload := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"rm internal/a.go internal/b.go"}}]}}`)

	got := ExtractActions(row, payload, "")
	paths := map[string]bool{}
	for _, a := range got {
		if a.ToolName != "Bash" {
			t.Errorf("expected Bash actions, got %q", a.ToolName)
		}
		paths[a.FilePath] = true
		if !hasSource(a, "file_path:payload") {
			t.Errorf("expected file_path:payload source, got %v", a.Sources)
		}
	}
	if !paths["internal/a.go"] || !paths["internal/b.go"] {
		t.Errorf("expected both rm targets in actions, got %v", paths)
	}
}

// Bash without a concrete path still produces one unknown-path action.
func TestExtractActions_BashWithoutDerivablePathEmitsUnknownAction(t *testing.T) {
	row := ActionEventRow{
		TurnID:   "t-1",
		ToolUses: `{"tools":[{"name":"Bash","file_op":"exec"}]}`,
	}
	payload := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo hello"}}]}}`)

	got := ExtractActions(row, payload, "")
	if len(got) != 1 {
		t.Fatalf("expected exactly one fallback action, got %d", len(got))
	}
	a := got[0]
	if a.ToolName != "Bash" || a.FilePath != "" {
		t.Errorf("expected Bash + empty path, got tool=%q file=%q", a.ToolName, a.FilePath)
	}
	if !hasSource(a, "file_path:unknown") {
		t.Errorf("expected file_path:unknown source, got %v", a.Sources)
	}
}

// Bash with a nil payload still produces one fallback action.
func TestExtractActions_BashWithNilPayloadStillProducesUnknownAction(t *testing.T) {
	row := ActionEventRow{
		TurnID:   "t-1",
		ToolUses: `{"tools":[{"name":"Bash","file_op":"exec"}]}`,
	}
	got := ExtractActions(row, nil, "")
	if len(got) != 1 || got[0].FilePath != "" || !hasSource(got[0], "file_path:unknown") {
		t.Errorf("expected one unknown-path Bash action, got %+v", got)
	}
}

// Read-only tools do not become agent actions.
func TestExtractActions_ReadOnlyToolsProduceNoActions(t *testing.T) {
	for _, tool := range []string{"Read", "Grep", "Glob", "WebFetch", "WebSearch", "TodoWrite"} {
		t.Run(tool, func(t *testing.T) {
			row := ActionEventRow{
				TurnID:   "t-1",
				ToolUses: `{"tools":[{"name":"` + tool + `"}]}`,
			}
			got := ExtractActions(row, nil, "")
			if len(got) != 0 {
				t.Errorf("read-only tool %q produced actions: %+v", tool, got)
			}
		})
	}
}

// Empty tool_uses returns no actions.
func TestExtractActions_EmptyToolUsesReturnsNil(t *testing.T) {
	if got := ExtractActions(ActionEventRow{TurnID: "t-1"}, nil, ""); got != nil {
		t.Errorf("empty tool_uses produced actions: %+v", got)
	}
}

// NeedsPayload is the loader's payload-fetch gate.
func TestNeedsPayload(t *testing.T) {
	cases := []struct {
		name     string
		toolUses string
		want     bool
	}{
		{"bash", `{"tools":[{"name":"Bash"}]}`, true},
		{"edit", `{"tools":[{"name":"Edit","file_path":"a.go"}]}`, false},
		{"write", `{"tools":[{"name":"Write","file_path":"a.go"}]}`, false},
		{"cursor", `{"tools":[{"name":"cursor_file_edit","file_path":"a.go"}]}`, false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NeedsPayload(tc.toolUses); got != tc.want {
				t.Errorf("NeedsPayload(%s) = %v, want %v", tc.toolUses, got, tc.want)
			}
		})
	}
}

// Action IDs are deterministic across identical anchors, and
// different anchors produce different IDs.
func TestDeriveActionID_DeterministicAndCollisionFree(t *testing.T) {
	a := deriveActionID("ev-1", 0, "t-1", "Edit", "a.go", 0, 0)
	b := deriveActionID("ev-1", 0, "t-1", "Edit", "a.go", 0, 0)
	if a != b {
		t.Errorf("ID not deterministic: %q vs %q", a, b)
	}
	c := deriveActionID("ev-1", 0, "t-1", "Edit", "b.go", 0, 0)
	if a == c {
		t.Errorf("different file produced same id: %q", a)
	}
	d := deriveActionID("ev-1", 0, "t-2", "Edit", "a.go", 0, 0)
	if a == d {
		t.Errorf("different turn produced same id: %q", a)
	}
	if !strings.HasPrefix(a, "a_") || len(a) != 18 {
		t.Errorf("id shape unexpected: %q (want a_ + 16 hex)", a)
	}
}

// Actions extracted from the same event remain distinct even when
// their other anchors match.
func TestDeriveActionID_SameAnchorsDifferentIndexDoNotCollide(t *testing.T) {
	a := deriveActionID("ev-1", 0, "t-1", "Edit", "a.go", 0, 0)
	b := deriveActionID("ev-1", 1, "t-1", "Edit", "a.go", 0, 0)
	if a == b {
		t.Errorf("same anchors at different index collided: %q", a)
	}
	c := deriveActionID("ev-2", 0, "t-1", "Edit", "a.go", 0, 0)
	if a == c {
		t.Errorf("same anchors in different events collided: %q", a)
	}
}

// Mixed tool_uses envelopes only emit mutating entries.
func TestExtractActions_MixedEnvelopeDropsReadOnly(t *testing.T) {
	row := ActionEventRow{
		EventID:  "ev-1",
		TurnID:   "t-1",
		ToolUses: `{"tools":[{"name":"Edit","file_path":"a.go"},{"name":"Read","file_path":"a.go"}]}`,
	}
	got := ExtractActions(row, nil, "")
	if len(got) != 1 {
		t.Fatalf("expected 1 action (Edit only), got %d: %+v", len(got), got)
	}
	if got[0].ToolName != "Edit" {
		t.Errorf("expected Edit, got %q (Read leaked through)", got[0].ToolName)
	}
}

// Compound Bash commands only emit concrete rm targets.
func TestExtractActions_BashCompoundCommandOnlyEmitsRMTargets(t *testing.T) {
	row := ActionEventRow{
		EventID:  "ev-1",
		TurnID:   "t-1",
		ToolUses: `{"tools":[{"name":"Bash","file_op":"exec"}]}`,
	}
	payload := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"rm internal/a.go internal/b.go && ls internal/a*"}}]}}`)

	got := ExtractActions(row, payload, "")
	paths := map[string]bool{}
	for _, a := range got {
		paths[a.FilePath] = true
	}
	if !paths["internal/a.go"] || !paths["internal/b.go"] {
		t.Errorf("missing rm targets in actions: %v", paths)
	}
	for _, bogus := range []string{"&&", "ls", "internal/a*"} {
		if paths[bogus] {
			t.Errorf("bogus path %q leaked into actions: %v", bogus, paths)
		}
	}
	if len(got) != 2 {
		t.Errorf("expected exactly 2 actions (the two rm targets), got %d: %+v", len(got), got)
	}
}

// Glob patterns are not concrete paths and must not become actions.
func TestExtractActions_BashGlobsAreDropped(t *testing.T) {
	row := ActionEventRow{
		EventID:  "ev-1",
		TurnID:   "t-1",
		ToolUses: `{"tools":[{"name":"Bash","file_op":"exec"}]}`,
	}
	payload := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"rm internal/*.tmp"}}]}}`)

	got := ExtractActions(row, payload, "")
	if len(got) != 1 || got[0].FilePath != "" || !hasSource(got[0], "file_path:unknown") {
		t.Errorf("expected single unknown-path action when only glob was present, got %+v", got)
	}
}

// Multiple rm statements chained with `;` each contribute targets.
func TestExtractActions_BashSemicolonChainedRMStatements(t *testing.T) {
	row := ActionEventRow{
		EventID:  "ev-1",
		TurnID:   "t-1",
		ToolUses: `{"tools":[{"name":"Bash","file_op":"exec"}]}`,
	}
	payload := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"rm a.go; rm b.go"}}]}}`)

	got := ExtractActions(row, payload, "")
	paths := map[string]bool{}
	for _, a := range got {
		paths[a.FilePath] = true
	}
	if !paths["a.go"] || !paths["b.go"] {
		t.Errorf("expected both rm targets across chained statements, got %v", paths)
	}
}

// Ambiguous rm segments produce an unknown-path action instead of
// trying to salvage clean-looking tokens.
func TestExtractActions_BashAmbiguousSegmentEmitsOnlyUnknownAction(t *testing.T) {
	cases := map[string]string{
		"command substitution": "rm $(find . -name a.go)",
		"backtick eval":        "rm `find . -name a.go`",
		"quoted target":        `rm "file with space.go"`,
		"stdout redirection":   "rm a.go > log.txt",
		"stderr redirection":   "rm a.go 2>/dev/null",
		"glob":                 "rm internal/*.tmp",
	}
	for name, cmd := range cases {
		t.Run(name, func(t *testing.T) {
			row := ActionEventRow{
				EventID:  "ev-1",
				TurnID:   "t-1",
				ToolUses: `{"tools":[{"name":"Bash","file_op":"exec"}]}`,
			}
			payload := []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":` + jsonEscape(cmd) + `}}]}}`)
			got := ExtractActions(row, payload, "")

			if len(got) != 1 {
				t.Fatalf("expected exactly one fallback action for ambiguous segment, got %d: %+v", len(got), got)
			}
			a := got[0]
			if a.FilePath != "" {
				t.Errorf("expected empty FilePath, got %q (ambiguous segment leaked a concrete path)", a.FilePath)
			}
			if !hasSource(a, "file_path:unknown") {
				t.Errorf("expected file_path:unknown source, got %v", a.Sources)
			}
		})
	}
}

// jsonEscape produces a JSON string literal for payload fixtures.
func jsonEscape(s string) string {
	r := strings.ReplaceAll(s, `\`, `\\`)
	r = strings.ReplaceAll(r, `"`, `\"`)
	return `"` + r + `"`
}

// Absolute paths are relativized against repoRoot before becoming
// action paths.
func TestExtractActions_AbsolutePathRelativizedAgainstRepoRoot(t *testing.T) {
	row := ActionEventRow{
		EventID:  "ev-1",
		TurnID:   "t-1",
		ToolUses: `{"tools":[{"name":"Edit","file_path":"/workspace/semantica/internal/handler.go"}]}`,
	}
	got := ExtractActions(row, nil, "/workspace/semantica")
	if len(got) != 1 {
		t.Fatalf("expected 1 action, got %d", len(got))
	}
	if got[0].FilePath != "internal/handler.go" {
		t.Errorf("FilePath = %q, want internal/handler.go (absolute path not relativized)", got[0].FilePath)
	}
}

// hasSource asserts Sources contents without depending on ordering.
func hasSource(a BundleAgentAction, want string) bool {
	for _, s := range a.Sources {
		if s == want {
			return true
		}
	}
	return false
}

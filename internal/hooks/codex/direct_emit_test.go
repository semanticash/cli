package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/semanticash/cli/internal/attribution/events"
	"github.com/semanticash/cli/internal/hooks"
)

// memBlobStore is an in-memory BlobPutter that hashes inputs the same
// way the production blob store does (sha256, hex-encoded) and keeps
// the bytes around so tests can fetch them back by hash and exercise
// the downstream extractor.
type memBlobStore struct {
	mu    sync.Mutex
	blobs map[string][]byte
}

func newMemBlobStore() *memBlobStore {
	return &memBlobStore{blobs: make(map[string][]byte)}
}

func (m *memBlobStore) Put(_ context.Context, b []byte) (string, int64, error) {
	sum := sha256.Sum256(b)
	hash := hex.EncodeToString(sum[:])
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.blobs[hash]; !exists {
		clone := make([]byte, len(b))
		copy(clone, b)
		m.blobs[hash] = clone
	}
	return hash, int64(len(b)), nil
}

func (m *memBlobStore) Get(hash string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blobs[hash]
	return b, ok
}

// applyPatchEvent builds the hooks.Event a real PostToolUse[apply_patch]
// hook delivery would produce, with the supplied envelope as the
// tool_input.command.
func applyPatchEvent(envelope, cwd string) *hooks.Event {
	input, _ := json.Marshal(map[string]string{"command": envelope})
	return &hooks.Event{
		Type:          hooks.ToolStepCompleted,
		SessionID:     "session-xyz",
		TranscriptRef: fixtureTranscript,
		Model:         "gpt-5.4",
		Timestamp:     1700000000000,
		CWD:           cwd,
		TurnID:        "turn-abc",
		ToolName:      "apply_patch",
		ToolInput:     input,
		ToolResponse:  json.RawMessage(`{"output":"Success","metadata":{"exit_code":0}}`),
		ToolUseID:     "call_test1",
	}
}

func TestBuildHookEvents_ApplyPatchAddProducesExtractableBlob(t *testing.T) {
	envelope := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: " + abs("tmp", "codex-fixture", "repo", "main.go"),
		"+package main",
		"+",
		"+func main() {",
		"+\tprintln(\"hi\")",
		"+}",
		"*** End Patch",
	}, "\n")
	event := applyPatchEvent(envelope, fixtureRepo)
	bs := newMemBlobStore()

	p := &Provider{}
	out, err := p.BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("BuildHookEvents: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d RawEvents, want 1", len(out))
	}
	ev := out[0]
	if ev.Provider != "codex" {
		t.Errorf("Provider = %q, want codex", ev.Provider)
	}
	if ev.PayloadHash == "" {
		t.Fatal("PayloadHash empty: scorer would route the file to provider-touch instead of line-level")
	}
	if ev.ToolUsesJSON == "" || !strings.Contains(ev.ToolUsesJSON, "main.go") {
		t.Errorf("ToolUsesJSON missing file_path: %q", ev.ToolUsesJSON)
	}
	if len(ev.FilePaths) != 1 || ev.FilePaths[0] != "main.go" {
		t.Errorf("FilePaths = %v, want [main.go]", ev.FilePaths)
	}

	// Load the stored blob and exercise the actual scorer-side
	// extractor. The line set it produces must include every line in
	// the patch envelope so the matcher credits this file as
	// line-level evidence rather than provider-touch.
	blob, ok := bs.Get(ev.PayloadHash)
	if !ok {
		t.Fatalf("blob %q not found in test store", ev.PayloadHash)
	}
	fileLines, _ := events.ExtractClaudeActions(blob, fixtureRepo)
	lines, ok := fileLines["main.go"]
	if !ok {
		t.Fatalf("extractor did not surface main.go; got keys %v", keysOf(fileLines))
	}
	// Extracted lines are TrimSpace'd by events.AddLines before
	// landing in the candidate set, so the leading indentation is
	// gone by the time the scorer matches against the diff (which is
	// trimmed the same way upstream).
	for _, expect := range []string{"package main", "func main() {", "println(\"hi\")", "}"} {
		if _, present := lines[expect]; !present {
			t.Errorf("expected line %q missing from extracted set %v", expect, keysOf2(lines))
		}
	}
}

func TestBuildHookEvents_ApplyPatchUpdateOnlyAddsPlusLines(t *testing.T) {
	envelope := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: main.go",
		"@@",
		" package main",
		"-\tprintln(\"old\")",
		"+\tprintln(\"new\")",
		"*** End Patch",
	}, "\n")
	event := applyPatchEvent(envelope, fixtureRepo)
	bs := newMemBlobStore()

	out, err := (&Provider{}).BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("BuildHookEvents: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	blob, _ := bs.Get(out[0].PayloadHash)
	fileLines, _ := events.ExtractClaudeActions(blob, fixtureRepo)
	lines := fileLines["main.go"]
	if _, ok := lines["println(\"new\")"]; !ok {
		t.Errorf("added line missing: %v", keysOf2(lines))
	}
	if _, ok := lines["println(\"old\")"]; ok {
		t.Errorf("removed (-) line should not appear in candidate set: %v", keysOf2(lines))
	}
	if _, ok := lines["package main"]; ok {
		t.Errorf("context line should not appear in candidate set: %v", keysOf2(lines))
	}
}

func TestBuildHookEvents_ApplyPatchDeleteEmitsFileTouchWithoutPayload(t *testing.T) {
	envelope := strings.Join([]string{
		"*** Begin Patch",
		"*** Delete File: " + abs("tmp", "codex-fixture", "repo", "legacy.go"),
		"*** End Patch",
	}, "\n")
	event := applyPatchEvent(envelope, fixtureRepo)
	bs := newMemBlobStore()

	out, err := (&Provider{}).BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("BuildHookEvents: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	ev := out[0]
	if ev.PayloadHash != "" {
		t.Errorf("Delete should not synthesize an assistant payload; got hash %q", ev.PayloadHash)
	}
	if len(ev.FilePaths) != 1 || ev.FilePaths[0] != "legacy.go" {
		t.Errorf("FilePaths = %v, want [legacy.go]", ev.FilePaths)
	}
}

func TestBuildHookEvents_ApplyPatchMoveYieldsDistinctEventIDs(t *testing.T) {
	// Move emits two records from one apply_patch call. Each half must
	// get a distinct stable identity so the broker keeps both records.
	envelope := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: old/path.go",
		"*** Move to: new/path.go",
		"@@",
		"+package newpath",
		"*** End Patch",
	}, "\n")
	event := applyPatchEvent(envelope, fixtureRepo)
	bs := newMemBlobStore()

	out, err := (&Provider{}).BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("BuildHookEvents: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d events, want 2 (source delete + dest add)", len(out))
	}
	if out[0].EventID == out[1].EventID {
		t.Errorf("move source and destination collide on EventID (%q); broker insert-or-ignore would drop one half", out[0].EventID)
	}
	if out[0].ToolUseID == out[1].ToolUseID {
		t.Errorf("move source and destination collide on ToolUseID (%q)", out[0].ToolUseID)
	}
}

func TestBuildHookEvents_ApplyPatchMoveSplitsIntoDeleteAndAdd(t *testing.T) {
	envelope := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: old/path.go",
		"*** Move to: new/path.go",
		"@@",
		"+package newpath",
		"*** End Patch",
	}, "\n")
	event := applyPatchEvent(envelope, fixtureRepo)
	bs := newMemBlobStore()

	out, err := (&Provider{}).BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("BuildHookEvents: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("Move should produce delete+add (2 events); got %d", len(out))
	}
	// Order is determined by buildApplyPatchEvents: the source delete
	// comes before the destination add.
	if out[0].FilePaths[0] != "old/path.go" || out[0].PayloadHash != "" {
		t.Errorf("first event should be delete of old/path.go without payload; got %+v", out[0])
	}
	if out[1].FilePaths[0] != "new/path.go" || out[1].PayloadHash == "" {
		t.Errorf("second event should be add of new/path.go with payload; got %+v", out[1])
	}
}

func TestBuildHookEvents_EmptyContentEventLandsInProviderTouchedFiles(t *testing.T) {
	// Empty-content patch records must use a provider-touch tool shape.
	// BuildCandidatesFromRows reads ToolUses rather than FilePaths for
	// this path, and assistant Write events without a PayloadHash stop
	// before candidate extraction.
	envelope := strings.Join([]string{
		"*** Begin Patch",
		"*** Delete File: legacy.go",
		"*** End Patch",
	}, "\n")
	hookEvent := applyPatchEvent(envelope, fixtureRepo)
	out, err := (&Provider{}).BuildHookEvents(context.Background(), hookEvent, newMemBlobStore())
	if err != nil {
		t.Fatalf("BuildHookEvents: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d emitted events, want 1", len(out))
	}

	// Translate the RawEvent into the EventRow shape the scorer's
	// candidate builder consumes. The mapping mirrors what the
	// production routing layer does when it loads rows from the
	// event store.
	rows := []events.EventRow{{
		Provider:    out[0].Provider,
		Role:        out[0].Role,
		ToolUses:    out[0].ToolUsesJSON,
		PayloadHash: out[0].PayloadHash,
		Payload:     nil,
		Model:       out[0].Model,
	}}

	cands, _ := events.BuildCandidatesFromRows(rows, fixtureRepo, nil)
	if got, ok := cands.ProviderTouchedFiles["legacy.go"]; !ok || got != "codex" {
		t.Errorf("legacy.go missing or wrong provider in ProviderTouchedFiles: got %q (present=%v); want %q", got, ok, "codex")
	}
	if _, ok := cands.AILines["legacy.go"]; ok {
		t.Errorf("legacy.go should not contribute line-level evidence; AILines = %v", cands.AILines)
	}
}

func TestBuildHookEvents_ApplyPatchEmptyContentStillEmitsTouchEvent(t *testing.T) {
	// Add/Update sections with no new content still touch a file. They
	// should emit provider-touch evidence rather than disappear from
	// attribution.
	cases := []struct {
		name     string
		envelope string
		wantPath string
	}{
		{
			name: "empty-file Add",
			envelope: strings.Join([]string{
				"*** Begin Patch",
				"*** Add File: blank.go",
				"*** End Patch",
			}, "\n"),
			wantPath: "blank.go",
		},
		{
			name: "deletion-only Update",
			envelope: strings.Join([]string{
				"*** Begin Patch",
				"*** Update File: shrink.go",
				"@@",
				" package shrink",
				"-removed line",
				"*** End Patch",
			}, "\n"),
			wantPath: "shrink.go",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event := applyPatchEvent(tc.envelope, fixtureRepo)
			bs := newMemBlobStore()
			out, err := (&Provider{}).BuildHookEvents(context.Background(), event, bs)
			if err != nil {
				t.Fatalf("BuildHookEvents: %v", err)
			}
			if len(out) != 1 {
				t.Fatalf("got %d events, want 1 (file-touched even without line-level content)", len(out))
			}
			ev := out[0]
			if len(ev.FilePaths) != 1 || ev.FilePaths[0] != tc.wantPath {
				t.Errorf("FilePaths = %v, want [%s]", ev.FilePaths, tc.wantPath)
			}
			if ev.PayloadHash != "" {
				t.Errorf("payload hash should be empty when no new content was added; got %q", ev.PayloadHash)
			}
		})
	}
}

func TestBuildHookEvents_ApplyPatchMultiFileYieldsDistinctEventIDs(t *testing.T) {
	envelope := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: a.go",
		"+package a",
		"*** Add File: b.go",
		"+package b",
		"*** End Patch",
	}, "\n")
	event := applyPatchEvent(envelope, fixtureRepo)
	bs := newMemBlobStore()

	out, err := (&Provider{}).BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("BuildHookEvents: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d events, want 2", len(out))
	}
	if out[0].EventID == out[1].EventID {
		t.Errorf("per-file events collide on EventID (%q); INSERT-OR-IGNORE would drop one", out[0].EventID)
	}
	if out[0].ToolUseID == out[1].ToolUseID {
		t.Errorf("per-file ToolUseID must differ; got %q for both", out[0].ToolUseID)
	}
}

func TestBuildHookEvents_BashStoresRedactedCommand(t *testing.T) {
	input, _ := json.Marshal(map[string]string{"command": "rm legacy.go"})
	event := &hooks.Event{
		Type:          hooks.ToolStepCompleted,
		SessionID:     "session-xyz",
		TranscriptRef: fixtureTranscriptAlt,
		Model:         "gpt-5.4",
		Timestamp:     1700000000000,
		CWD:           fixtureRepo,
		TurnID:        "turn-abc",
		ToolName:      "Bash",
		ToolInput:     input,
		ToolResponse:  json.RawMessage(`""`),
		ToolUseID:     "call_bash",
	}
	bs := newMemBlobStore()

	out, err := (&Provider{}).BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("BuildHookEvents: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	ev := out[0]
	if ev.ToolName != "Bash" {
		t.Errorf("ToolName = %q, want Bash", ev.ToolName)
	}
	if !strings.Contains(ev.ToolUsesJSON, "Bash") {
		t.Errorf("ToolUsesJSON missing Bash marker: %q", ev.ToolUsesJSON)
	}
	if ev.PayloadHash == "" {
		t.Error("Bash payload hash should be set for scorer recognition")
	}
	blob, _ := bs.Get(ev.PayloadHash)
	_, bash := events.ExtractClaudeActions(blob, fixtureRepo)
	if len(bash) != 1 || bash[0] != "rm legacy.go" {
		t.Errorf("extracted bash commands = %v, want [rm legacy.go]", bash)
	}
}

func TestBuildHookEvents_PromptSubmittedStoresPromptBlob(t *testing.T) {
	event := &hooks.Event{
		Type:          hooks.PromptSubmitted,
		SessionID:     "session-xyz",
		TranscriptRef: fixtureTranscriptAlt,
		Model:         "gpt-5.4",
		Timestamp:     1700000000000,
		Prompt:        "Refactor the scoring module",
		CWD:           fixtureRepo,
		TurnID:        "turn-abc",
	}
	bs := newMemBlobStore()
	out, err := (&Provider{}).BuildHookEvents(context.Background(), event, bs)
	if err != nil {
		t.Fatalf("BuildHookEvents: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	ev := out[0]
	if ev.Kind != "user" {
		t.Errorf("Kind = %q, want user", ev.Kind)
	}
	if ev.PayloadHash == "" {
		t.Error("PayloadHash empty: prompt blob not stored")
	}
	blob, ok := bs.Get(ev.PayloadHash)
	if !ok || string(blob) != event.Prompt {
		t.Errorf("blob = %q, want %q", string(blob), event.Prompt)
	}
}

func TestBuildHookEvents_LifecycleEventsAreNoOp(t *testing.T) {
	for _, ty := range []hooks.EventType{hooks.SessionOpened, hooks.AgentCompleted, hooks.SessionClosed} {
		event := &hooks.Event{Type: ty}
		out, err := (&Provider{}).BuildHookEvents(context.Background(), event, newMemBlobStore())
		if err != nil {
			t.Errorf("event type %v returned err: %v", ty, err)
		}
		if len(out) != 0 {
			t.Errorf("event type %v emitted %d events through DirectHookEmitter; lifecycle goes via the dispatcher cases", ty, len(out))
		}
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keysOf2(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

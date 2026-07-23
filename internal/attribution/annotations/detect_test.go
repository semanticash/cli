package annotations

import (
	"encoding/json"
	"testing"
)

const repoRoot = "/repo"

// --- payload builders -------------------------------------------------------

type toolUse struct {
	Type  string         `json:"type"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// claudePayload builds a Claude-format assistant payload from tool uses.
func claudePayload(t *testing.T, tools ...toolUse) []byte {
	t.Helper()
	content := make([]json.RawMessage, 0, len(tools))
	for _, tu := range tools {
		tu.Type = "tool_use"
		b, err := json.Marshal(tu)
		if err != nil {
			t.Fatalf("marshal tool use: %v", err)
		}
		content = append(content, b)
	}
	env := map[string]any{
		"type":    "assistant",
		"message": map[string]any{"content": content},
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

func editTool(path, oldStr, newStr string) toolUse {
	return toolUse{Name: "Edit", Input: map[string]any{
		"file_path": path, "old_string": oldStr, "new_string": newStr,
	}}
}

func writeTool(path, content string) toolUse {
	return toolUse{Name: "Write", Input: map[string]any{
		"file_path": path, "content": content,
	}}
}

func bashTool(cmd string) toolUse {
	return toolUse{Name: "Bash", Input: map[string]any{"command": cmd}}
}

// claudeEvent builds an assistant Event carrying the given tool uses.
func claudeEvent(t *testing.T, id, turn string, ts int64, tools ...toolUse) Event {
	return Event{
		EventID:        id,
		TurnID:         turn,
		Provider:       "claude_code",
		TS:             ts,
		Role:           "assistant",
		Payload:        claudePayload(t, tools...),
		ProvenanceHash: "sha256:" + id,
	}
}

// providerTouchEvent builds a provider file-touch Event (no line payload).
func providerTouchEvent(id, turn string, ts int64, toolName, path string) Event {
	tu, _ := json.Marshal(map[string]any{
		"tools": []map[string]any{{"name": toolName, "file_path": path}},
	})
	return Event{
		EventID:  id,
		TurnID:   turn,
		Provider: "cursor",
		TS:       ts,
		Role:     "user",
		ToolUses: string(tu),
	}
}

func files(paths ...string) map[string]bool {
	m := map[string]bool{}
	for _, p := range paths {
		m[p] = true
	}
	return m
}

// revokeBlock is a three-line substantive block used across rework cases.
const revokeBlock = "func revokeToken(id string) error {\n" +
	"    store.Delete(id)\n" +
	"    return errors.New(\"revoked\")\n" +
	"}"

const transactionalBlock = "func revokeToken(id string) error {\n" +
	"    tx := store.Begin()\n" +
	"    tx.Delete(id)\n" +
	"    return tx.Commit()\n" +
	"}"

// --- helpers ----------------------------------------------------------------

func only(t *testing.T, anns []Annotation, kind Kind) Annotation {
	t.Helper()
	var found []Annotation
	for _, a := range anns {
		if a.Kind == kind {
			found = append(found, a)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly 1 %s annotation, got %d (%d total)", kind, len(found), len(anns))
	}
	return found[0]
}

func assertNone(t *testing.T, anns []Annotation) {
	t.Helper()
	if len(anns) != 0 {
		t.Fatalf("want no annotations, got %d: %+v", len(anns), anns)
	}
}

// --- possible_rework --------------------------------------------------------

func TestRework_AgentRevisesOwnEditAcrossTurns(t *testing.T) {
	path := "internal/auth/token.go"
	in := DetectInput{
		CommitSHA: "a13f92c",
		RepoRoot:  repoRoot,
		Events: []Event{
			claudeEvent(t, "e1", "t1", 100, writeTool(path, revokeBlock)),
			claudeEvent(t, "e2", "t2", 200, editTool(path, revokeBlock, transactionalBlock)),
		},
		Commit: CommitDiff{Files: files(path)},
	}

	ann := only(t, Detect(in), KindPossibleRework)
	if ann.FilePath != path {
		t.Errorf("file_path = %q, want %q", ann.FilePath, path)
	}
	if ann.LineStart != 0 || ann.LineEnd != 0 {
		t.Errorf("v1 rework is file-precise, want zero line range, got [%d,%d]", ann.LineStart, ann.LineEnd)
	}
	if len(ann.TurnIDs) != 2 {
		t.Errorf("want both turns anchored, got %v", ann.TurnIDs)
	}
	if len(ann.SupportingStepRefs) != 2 {
		t.Errorf("want 2 supporting step refs, got %d", len(ann.SupportingStepRefs))
	}
	if ann.Status != StatusComplete || ann.AlgorithmVersion != AlgorithmVersion {
		t.Errorf("status/version = %q/%q", ann.Status, ann.AlgorithmVersion)
	}
	if ann.StartedAt != 100 || ann.EndedAt != 200 {
		t.Errorf("window = [%d,%d], want [100,200]", ann.StartedAt, ann.EndedAt)
	}
}

func TestRework_NotDetected_WriteThenKept(t *testing.T) {
	path := "internal/auth/token.go"
	in := DetectInput{
		CommitSHA: "a13f92c",
		RepoRoot:  repoRoot,
		Events: []Event{
			claudeEvent(t, "e1", "t1", 100, writeTool(path, revokeBlock)),
		},
		Commit: CommitDiff{Files: files(path)},
	}
	assertNone(t, Detect(in))
}

func TestRework_NotDetected_SameTurnReEdit(t *testing.T) {
	path := "internal/auth/token.go"
	in := DetectInput{
		CommitSHA: "a13f92c",
		RepoRoot:  repoRoot,
		Events: []Event{
			claudeEvent(t, "e1", "t1", 100, writeTool(path, revokeBlock)),
			// same turn -> ordinary multi-step authoring, not rework
			claudeEvent(t, "e2", "t1", 150, editTool(path, revokeBlock, transactionalBlock)),
		},
		Commit: CommitDiff{Files: files(path)},
	}
	assertNone(t, Detect(in))
}

func TestRework_NotDetected_UnrelatedHunks(t *testing.T) {
	path := "internal/auth/token.go"
	other := "type Config struct {\n    Host string\n    Port int\n}"
	in := DetectInput{
		CommitSHA: "a13f92c",
		RepoRoot:  repoRoot,
		Events: []Event{
			claudeEvent(t, "e1", "t1", 100, writeTool(path, revokeBlock)),
			// later edit replaces content the agent never authored -> no overlap
			claudeEvent(t, "e2", "t2", 200, editTool(path, other, other+"\n    TLS bool")),
		},
		Commit: CommitDiff{Files: files(path)},
	}
	assertNone(t, Detect(in))
}

func TestRework_NotDetected_TwoAgentsProviderTouch(t *testing.T) {
	path := "internal/auth/token.go"
	in := DetectInput{
		CommitSHA: "a13f92c",
		RepoRoot:  repoRoot,
		Events: []Event{
			claudeEvent(t, "e1", "t1", 100, writeTool(path, revokeBlock)),
			// a provider-only touch carries no line content, so it can never
			// establish structural overlap
			providerTouchEvent("e2", "t2", 200, "cursor_file_edit", path),
		},
		Commit: CommitDiff{Files: files(path)},
	}
	assertNone(t, Detect(in))
}

func TestRework_NotDetected_FileNotInCommit(t *testing.T) {
	path := "internal/auth/token.go"
	in := DetectInput{
		CommitSHA: "a13f92c",
		RepoRoot:  repoRoot,
		Events: []Event{
			claudeEvent(t, "e1", "t1", 100, writeTool(path, revokeBlock)),
			claudeEvent(t, "e2", "t2", 200, editTool(path, revokeBlock, transactionalBlock)),
		},
		Commit: CommitDiff{Files: files("other.go")}, // reworked file never landed
	}
	assertNone(t, Detect(in))
}

// --- attempted_removed ------------------------------------------------------

func TestRemoved_AgentCreatedThenRm(t *testing.T) {
	path := "db/migrations/old_migration.sql"
	in := DetectInput{
		CommitSHA: "44e890f",
		RepoRoot:  repoRoot,
		Events: []Event{
			claudeEvent(t, "e1", "t1", 100, writeTool(path, "create table foo (id int);\ninsert into foo values (1);\nselect * from foo;")),
			claudeEvent(t, "e2", "t2", 200, bashTool("rm "+path)),
		},
		Commit: CommitDiff{Files: files("db/migrations/new_migration.sql")},
	}
	ann := only(t, Detect(in), KindAttemptedRemoved)
	if ann.FilePath != path {
		t.Errorf("file_path = %q, want %q", ann.FilePath, path)
	}
	if ann.Confidence != 0.6 {
		t.Errorf("rm-recognized confidence = %v, want 0.6", ann.Confidence)
	}
	if len(ann.TurnIDs) != 2 {
		t.Errorf("want authoring + removal turns, got %v", ann.TurnIDs)
	}
}

func TestRemoved_CommitDeletesAgentFile(t *testing.T) {
	path := "internal/legacy/shim.go"
	in := DetectInput{
		CommitSHA: "a13f92c",
		RepoRoot:  repoRoot,
		Events: []Event{
			claudeEvent(t, "e1", "t1", 100, editTool(path, "old code here now", "newer code here now")),
		},
		Commit: CommitDiff{
			Files:        files(path),
			FilesDeleted: files(path),
		},
	}
	ann := only(t, Detect(in), KindAttemptedRemoved)
	if ann.Confidence != 0.85 {
		t.Errorf("commit-delete confidence = %v, want 0.85", ann.Confidence)
	}
}

func TestRemoved_NotDetected_HumanDeletion(t *testing.T) {
	// File deleted by the commit but never touched by the agent.
	in := DetectInput{
		CommitSHA: "a13f92c",
		RepoRoot:  repoRoot,
		Events: []Event{
			claudeEvent(t, "e1", "t1", 100, writeTool("internal/auth/token.go", revokeBlock)),
		},
		Commit: CommitDiff{
			Files:        files("internal/auth/token.go", "internal/legacy/gone.go"),
			FilesDeleted: files("internal/legacy/gone.go"),
		},
	}
	for _, a := range Detect(in) {
		if a.Kind == KindAttemptedRemoved {
			t.Fatalf("human deletion must not be annotated: %+v", a)
		}
	}
}

func TestRemoved_NotDetected_RenameOnly(t *testing.T) {
	// Rename shows as delete+create in the diff with no agent involvement.
	in := DetectInput{
		CommitSHA: "a13f92c",
		RepoRoot:  repoRoot,
		Events:    nil,
		Commit: CommitDiff{
			Files:        files("internal/new/name.go"),
			FilesDeleted: files("internal/old/name.go"),
		},
	}
	assertNone(t, Detect(in))
}

func TestRework_NotDetected_EqualTimestampWrongOrder(t *testing.T) {
	// The replacing edit appears first in the (ts, event_id)-ordered stream;
	// the authoring write comes second with the same millisecond. Strict
	// ordering should not treat the later-in-stream write as "earlier".
	path := "internal/auth/token.go"
	in := DetectInput{
		CommitSHA: "a13f92c",
		RepoRoot:  repoRoot,
		Events: []Event{
			claudeEvent(t, "e1", "t1", 100, editTool(path, revokeBlock, transactionalBlock)),
			claudeEvent(t, "e2", "t2", 100, writeTool(path, revokeBlock)),
		},
		Commit: CommitDiff{Files: files(path)},
	}
	assertNone(t, Detect(in))
}

func TestRemoved_MergesRmAndCommitDelete(t *testing.T) {
	// A file removed with rm and deleted by the commit yields one
	// annotation that carries both the rm command and the commit as evidence.
	path := "db/migrations/old.sql"
	in := DetectInput{
		CommitSHA: "44e890f",
		RepoRoot:  repoRoot,
		Events: []Event{
			claudeEvent(t, "e1", "t1", 100, writeTool(path, "create table foo (id int);\ninsert into foo values (1);\nselect * from foo;")),
			claudeEvent(t, "e2", "t2", 200, bashTool("rm "+path)),
		},
		Commit: CommitDiff{FilesDeleted: files(path)},
	}
	ann := only(t, Detect(in), KindAttemptedRemoved)
	if ann.Confidence != 0.85 {
		t.Errorf("commit-delete should win confidence, got %v", ann.Confidence)
	}
	// Both the authoring and the rm event must survive as evidence.
	if len(ann.SupportingStepRefs) != 2 {
		t.Errorf("want touch + rm step refs, got %d: %+v", len(ann.SupportingStepRefs), ann.SupportingStepRefs)
	}
	foundRm := false
	for _, ref := range ann.SupportingStepRefs {
		if ref.EventID == "e2" {
			foundRm = true
		}
	}
	if !foundRm {
		t.Error("rm command evidence (e2) was dropped")
	}
}

func TestStatus_PartialWhenTurnIDMissing(t *testing.T) {
	// A touched file whose event carries no turn_id cannot be resolved via
	// turn detail, so the annotation should not claim complete.
	path := "internal/legacy/shim.go"
	ev := claudeEvent(t, "e1", "", 100, writeTool(path, revokeBlock))
	in := DetectInput{
		CommitSHA: "a13f92c",
		RepoRoot:  repoRoot,
		Events:    []Event{ev},
		Commit:    CommitDiff{Files: files(path), FilesDeleted: files(path)},
	}
	ann := only(t, Detect(in), KindAttemptedRemoved)
	if ann.Status != StatusPartial {
		t.Errorf("status = %q, want %q (missing turn_id)", ann.Status, StatusPartial)
	}
}

// --- generic non-detection --------------------------------------------------

func TestNotDetected_UnknownToolShape(t *testing.T) {
	in := DetectInput{
		CommitSHA: "a13f92c",
		RepoRoot:  repoRoot,
		Events: []Event{
			claudeEvent(t, "e1", "t1", 100, toolUse{Name: "MysteryTool", Input: map[string]any{"foo": "bar"}}),
		},
		Commit: CommitDiff{Files: files("internal/auth/token.go")},
	}
	assertNone(t, Detect(in))
}

func TestNotDetected_EmptyInput(t *testing.T) {
	assertNone(t, Detect(DetectInput{CommitSHA: "a13f92c", RepoRoot: repoRoot}))
}

// --- determinism ------------------------------------------------------------

func TestDetect_DeterministicIDsAndOrder(t *testing.T) {
	path := "internal/auth/token.go"
	sql := "db/old.sql"
	in := DetectInput{
		CommitSHA: "a13f92c",
		RepoRoot:  repoRoot,
		Events: []Event{
			claudeEvent(t, "e1", "t1", 100, writeTool(path, revokeBlock)),
			claudeEvent(t, "e2", "t2", 200, editTool(path, revokeBlock, transactionalBlock)),
			claudeEvent(t, "e3", "t3", 300, writeTool(sql, "create table x (id int);\ninsert into x values (2);\nselect id from x;")),
			claudeEvent(t, "e4", "t4", 400, bashTool("rm "+sql)),
		},
		Commit: CommitDiff{Files: files(path)},
	}

	first := Detect(in)
	second := Detect(in)
	if len(first) != len(second) {
		t.Fatalf("nondeterministic count: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Errorf("nondeterministic ID at %d: %q vs %q", i, first[i].ID, second[i].ID)
		}
		if first[i].Kind != second[i].Kind {
			t.Errorf("nondeterministic order at %d: %q vs %q", i, first[i].Kind, second[i].Kind)
		}
	}
	// attempted_removed sorts before possible_rework by kind.
	if len(first) != 2 || first[0].Kind != KindAttemptedRemoved || first[1].Kind != KindPossibleRework {
		t.Fatalf("unexpected annotation set/order: %+v", first)
	}
}

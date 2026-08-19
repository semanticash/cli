package provenance

import (
	"encoding/json"
	"strings"
	"testing"

	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/toolsnap"
)

func link(kind, hash, group string) sqldb.AgentEventEvidenceLink {
	return sqldb.AgentEventEvidenceLink{EvidenceKind: kind, EvidenceHash: hash, GroupID: group}
}

// Events must map to exactly one tool-delta group.
func TestSelectStepDeltaHash(t *testing.T) {
	cases := []struct {
		name  string
		links []sqldb.AgentEventEvidenceLink
		want  string
		ok    bool
	}{
		{"none", nil, "", false},
		{"other-kinds-ignored", []sqldb.AgentEventEvidenceLink{link("something_else", "h", "g1")}, "", false},
		{"single-group", []sqldb.AgentEventEvidenceLink{link("tool_delta", "abc", "g1")}, "abc", true},
		{"cross-group-ambiguous", []sqldb.AgentEventEvidenceLink{
			link("tool_delta", "abc", "g1"), link("tool_delta", "def", "g2"),
		}, "", false},
		{"empty-hash", []sqldb.AgentEventEvidenceLink{link("tool_delta", "", "g1")}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := selectStepDeltaHash(tc.links)
			if got != tc.want || ok != tc.ok {
				t.Errorf("selectStepDeltaHash = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// Shared deltas are returned once.
func TestExtractStepDeltaHashes(t *testing.T) {
	bundle := []byte(`{"steps":[
		{"event_id":"e1","delta_hash":"aaa"},
		{"event_id":"e2","delta_hash":"aaa"},
		{"event_id":"e3","delta_hash":"bbb"},
		{"event_id":"e4"}
	]}`)
	got := extractStepDeltaHashes(bundle)
	if len(got) != 2 || got[0] != "aaa" || got[1] != "bbb" {
		t.Errorf("extractStepDeltaHashes = %v, want [aaa bbb]", got)
	}
}

const singleLineSecret = "sk-1234567890abcdef1234567890abcdef"

// Hunk redaction preserves line counts.
func TestRedactHunkLines_SingleLineSecret(t *testing.T) {
	lines := []string{"safe line", "api_key = " + singleLineSecret, "another safe line"}
	out, err := redactHunkLines(lines)
	if err != nil {
		t.Fatalf("redactHunkLines: %v", err)
	}
	if len(out) != len(lines) {
		t.Fatalf("line count changed: %d -> %d", len(lines), len(out))
	}
	if strings.Contains(strings.Join(out, "\n"), singleLineSecret) {
		t.Errorf("secret leaked: %v", out)
	}
	if !strings.Contains(out[1], "[REDACTED]") {
		t.Errorf("expected redaction marker: %v", out)
	}
	if out[0] != "safe line" || out[2] != "another safe line" {
		t.Errorf("non-secret lines altered: %v", out)
	}
}

// Lines without secrets remain unchanged.
func TestRedactHunkLines_NoSecret(t *testing.T) {
	lines := []string{"a", "b", "c"}
	out, err := redactHunkLines(lines)
	if err != nil {
		t.Fatalf("redactHunkLines: %v", err)
	}
	if strings.Join(out, "\n") != strings.Join(lines, "\n") {
		t.Errorf("lines changed: %v", out)
	}
}

// Secrets that span lines are rejected.
func TestRedactHunkLines_MultiLineSecretFailsClosed(t *testing.T) {
	lines := []string{
		"-----BEGIN RSA PRIVATE KEY-----",
		"MIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn/ygWyF8PbnGcY5unA67hFdJBEEH6kMRMD",
		"-----END RSA PRIVATE KEY-----",
	}
	if _, err := redactHunkLines(lines); err == nil {
		t.Fatal("expected fail-closed error for multi-line secret")
	}
}

// canonicalDelta returns a valid single-file delta.
func canonicalDelta(t *testing.T, commandSummary string, newLines []string) []byte {
	t.Helper()
	d := &toolsnap.Delta{
		Scope:    "tool",
		Status:   "complete",
		Window:   toolsnap.Window{StartedAt: 1, CompletedAt: 2, DurationMS: 1},
		Actors:   []toolsnap.Actor{{Provider: "codex", SessionID: "s1", TurnID: "t1"}},
		ToolUses: []toolsnap.ToolUse{{ToolUseID: "tu1", ToolName: "Bash", CommandSummary: commandSummary, EventID: "e1", Actor: 0}},
		Files: []toolsnap.FileDelta{{
			Path: "src/config.go", Operation: "edit",
			BeforeMode: "100644", AfterMode: "100644", BeforeHash: "aaa", AfterHash: "bbb",
			Hunks: []toolsnap.Hunk{{
				OldStart: 1, OldCount: 1, NewStart: 1, NewCount: len(newLines),
				OldLines: []string{"old"}, NewLines: newLines,
			}},
		}},
		Limits: toolsnap.Limits{FilesObserved: 1},
	}
	blob, err := d.CanonicalBytes()
	if err != nil {
		t.Fatalf("CanonicalBytes: %v", err)
	}
	return blob
}

// Redacted deltas remain canonical.
func TestRedactToolDelta(t *testing.T) {
	blob := canonicalDelta(t, "export KEY="+singleLineSecret, []string{"api_key = " + singleLineSecret})

	out, err := RedactForUpload(blob, "tool_delta", testRepoRoot)
	if err != nil {
		t.Fatalf("RedactForUpload: %v", err)
	}
	if strings.Contains(string(out), singleLineSecret) {
		t.Errorf("secret leaked: %s", out)
	}
	if !strings.Contains(string(out), "[REDACTED]") {
		t.Errorf("expected redaction marker: %s", out)
	}
	// Consumers that enforce canonical encoding must accept the result.
	if _, err := toolsnap.ParseDelta(out); err != nil {
		t.Errorf("redacted delta is not canonical: %v\n%s", err, out)
	}
}

// Non-canonical input is rejected.
func TestRedactToolDelta_RejectsNonCanonical(t *testing.T) {
	valid := canonicalDelta(t, "ok", []string{"new"})
	var m map[string]any
	if err := json.Unmarshal(valid, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m["unexpected_field"] = "x"
	tampered, _ := json.Marshal(m)
	if _, err := RedactForUpload(tampered, "tool_delta", testRepoRoot); err == nil {
		t.Fatal("expected non-canonical delta (unknown field) to be dropped")
	}
}

// Multi-line secrets reject the delta.
func TestRedactToolDelta_MultiLineSecretFailsClosed(t *testing.T) {
	blob := canonicalDelta(t, "", []string{
		"-----BEGIN RSA PRIVATE KEY-----",
		"MIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn/ygWyF8PbnGcY5unA67hFdJBEEH6kMRMD",
		"-----END RSA PRIVATE KEY-----",
	})
	if _, err := RedactForUpload(blob, "tool_delta", testRepoRoot); err == nil {
		t.Fatal("expected fail-closed error for multi-line secret in delta")
	}
}

// Mapped delta hashes are rewritten and unmapped hashes are removed.
func TestRewriteBundleHashes_DeltaHash(t *testing.T) {
	bundle := []byte(`{"prompt":{"blob_hash":"plocal"},"steps":[
		{"event_id":"e1","provenance_hash":"prov","delta_hash":"dlocal"},
		{"event_id":"e2","delta_hash":"dropped"}
	]}`)
	hashMap := map[string]string{
		"plocal": "pupload",
		"prov":   "provupload",
		"dlocal": "dupload",
		// "dropped" intentionally absent.
	}
	out := RewriteBundleHashes(bundle, hashMap)

	var parsed struct {
		Steps []map[string]json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var e1Delta string
	_ = json.Unmarshal(parsed.Steps[0]["delta_hash"], &e1Delta)
	if e1Delta != "dupload" {
		t.Errorf("e1 delta_hash = %q, want dupload", e1Delta)
	}
	if _, present := parsed.Steps[1]["delta_hash"]; present {
		t.Errorf("unmapped delta_hash should be stripped, got %s", parsed.Steps[1]["delta_hash"])
	}
}

// An empty hash map still removes delta references.
func TestRewriteBundleHashes_DeltaOnlyEmptyMap(t *testing.T) {
	bundle := []byte(`{"steps":[{"event_id":"e1","delta_hash":"dlocal"}]}`)
	out := RewriteBundleHashes(bundle, map[string]string{})

	var parsed struct {
		Steps []map[string]json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := parsed.Steps[0]["delta_hash"]; present {
		t.Errorf("dangling delta_hash should be stripped with an empty map, got %s", parsed.Steps[0]["delta_hash"])
	}
}

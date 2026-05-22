package provenance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/redact"
)

var (
	testRepoRoot          = filepath.Clean("/workspace/myrepo")
	testTranscriptRef     = filepath.Clean("/workspace/.claude/projects/transcript.jsonl")
	testRepoMainFile      = filepath.Join(testRepoRoot, "src", "main.go")
	testRepoConfigPath    = filepath.Join(testRepoRoot, "config.go")
	testGenericRepoRoot   = filepath.Clean("/workspace/repo")
	testGenericRepoMainGo = filepath.Join(testGenericRepoRoot, "main.go")
)

func TestRedactPrompt_PassesThrough(t *testing.T) {
	// Ordinary prompt text should remain readable and return bytes.
	blob := []byte(`Please deploy the service to staging.`)
	result, err := RedactForUpload(blob, "prompt", "/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) == 0 {
		t.Error("expected non-empty result")
	}
}

func TestRedactBundle_NormalizesPathsAndDropsTranscriptRef(t *testing.T) {
	bundle := map[string]any{
		"turn_id":        "t1",
		"cwd":            testRepoRoot,
		"transcript_ref": testTranscriptRef,
		"steps": []map[string]any{
			{
				"tool_name":  "Write",
				"file_paths": []string{testRepoMainFile},
			},
		},
	}
	blob, _ := json.Marshal(bundle)

	result, err := RedactForUpload(blob, "bundle", testRepoRoot)
	if err != nil {
		t.Fatal(err)
	}

	var out map[string]json.RawMessage
	if err := json.Unmarshal(result, &out); err != nil {
		t.Fatal(err)
	}

	// transcript_ref should be dropped.
	if _, ok := out["transcript_ref"]; ok {
		t.Error("expected transcript_ref to be dropped")
	}

	// cwd should be normalized.
	var cwd string
	_ = json.Unmarshal(out["cwd"], &cwd)
	if cwd == testRepoRoot {
		t.Errorf("cwd not normalized: %s", cwd)
	}

	// file_paths should be repo-relative.
	if strings.Contains(string(result), testRepoMainFile) {
		t.Error("expected absolute file path to be normalized to repo-relative")
	}
	if !strings.Contains(string(result), "src/main.go") {
		t.Error("expected repo-relative file path in output")
	}
}

func TestRedactStepProvenance_NormalizesAndRedacts(t *testing.T) {
	prov := map[string]any{
		"tool_input": map[string]any{
			"file_path": testRepoConfigPath,
			"command":   "export SECRET_KEY=sk-abc123 && go test",
		},
		"tool_response": map[string]any{
			"stdout": "PASS",
			"stderr": "",
		},
	}
	blob, _ := json.Marshal(prov)

	result, err := RedactForUpload(blob, "step_provenance", testRepoRoot)
	if err != nil {
		t.Fatal(err)
	}

	// Path should be normalized.
	if strings.Contains(string(result), testRepoConfigPath) {
		t.Error("expected absolute path to be normalized")
	}
	if !strings.Contains(string(result), "config.go") {
		t.Error("expected repo-relative path")
	}
}

// Secrets embedded in the wrapped tool_input envelope must be
// redacted before upload. This covers the shared single-file shape
// written by file-changing providers.
func TestRedactStepProvenance_RedactsWrappedToolInputFields(t *testing.T) {
	const fakeKey = "sk-1234567890abcdef1234567890abcdef"

	cases := []struct {
		name   string
		input  map[string]any
		secret string
	}{
		{
			name: "Write_content",
			input: map[string]any{
				"file_path": "src/main.go",
				"content":   "api_key = " + fakeKey,
			},
			secret: fakeKey,
		},
		{
			name: "Edit_old_string",
			input: map[string]any{
				"file_path":  "src/main.go",
				"old_string": "api_key = " + fakeKey,
				"new_string": "api_key = scrubbed",
			},
			secret: fakeKey,
		},
		{
			name: "Edit_new_string",
			input: map[string]any{
				"file_path":  "src/main.go",
				"old_string": "api_key = scrubbed",
				"new_string": "api_key = " + fakeKey,
			},
			secret: fakeKey,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := map[string]any{"tool_input": tc.input}
			blob, _ := json.Marshal(prov)

			result, err := RedactForUpload(blob, "step_provenance", testRepoRoot)
			if err != nil {
				t.Fatalf("RedactForUpload: %v", err)
			}
			if strings.Contains(string(result), tc.secret) {
				t.Errorf("secret leaked through wrapped envelope: %s", string(result))
			}
			if !strings.Contains(string(result), "[REDACTED]") {
				t.Errorf("expected redaction marker in output: %s", string(result))
			}
		})
	}
}

// Secrets embedded in canonical files[] entries must be redacted
// before upload.
func TestRedactStepProvenance_RedactsFilesArrayContent(t *testing.T) {
	const fakeKey = "sk-1234567890abcdef1234567890abcdef"
	prov := map[string]any{
		"version": 1,
		"files": []map[string]any{
			{
				"path":           "src/config.go",
				"operation":      "edit",
				"old_text":       "api_key = old",
				"new_text":       "api_key = " + fakeKey,
				"diff_available": true,
			},
			{
				"path":           "src/main.go",
				"operation":      "create",
				"content":        "api_key = " + fakeKey,
				"diff_available": true,
			},
		},
	}
	blob, _ := json.Marshal(prov)

	result, err := RedactForUpload(blob, "step_provenance", testRepoRoot)
	if err != nil {
		t.Fatalf("RedactForUpload: %v", err)
	}
	if strings.Contains(string(result), fakeKey) {
		t.Errorf("secret leaked into files[] content: %s", string(result))
	}
	if !strings.Contains(string(result), "[REDACTED]") {
		t.Errorf("expected redaction marker in output: %s", string(result))
	}
}

// Non-object entries inside files[] are scanned too. Canonical entries
// are objects, but malformed entries must not bypass redaction.
func TestRedactStepProvenance_RedactsFilesArrayStringEntry(t *testing.T) {
	const fakeKey = "sk-1234567890abcdef1234567890abcdef"
	prov := map[string]any{
		"files": []any{
			"api_key = " + fakeKey,
		},
	}
	blob, _ := json.Marshal(prov)

	result, err := RedactForUpload(blob, "step_provenance", testRepoRoot)
	if err != nil {
		t.Fatalf("RedactForUpload: %v", err)
	}
	if strings.Contains(string(result), fakeKey) {
		t.Errorf("secret leaked from non-object files[] entry: %s", string(result))
	}
}

// Path normalization inside files[] entries: an absolute repo path
// should land repo-relative in the uploaded shape.
func TestRedactStepProvenance_NormalizesFilesArrayPaths(t *testing.T) {
	prov := map[string]any{
		"files": []map[string]any{
			{"path": testRepoConfigPath, "operation": "edit", "old_text": "a", "new_text": "b"},
		},
	}
	blob, _ := json.Marshal(prov)

	result, err := RedactForUpload(blob, "step_provenance", testRepoRoot)
	if err != nil {
		t.Fatalf("RedactForUpload: %v", err)
	}
	if strings.Contains(string(result), testRepoConfigPath) {
		t.Errorf("absolute path leaked into files[] path: %s", string(result))
	}
	if !strings.Contains(string(result), "config.go") {
		t.Errorf("expected repo-relative path in files[] entry: %s", string(result))
	}
}

func TestRedactStepProvenance_DropsTopLevelLocalPaths(t *testing.T) {
	prov := map[string]any{
		"cwd":             testRepoRoot,
		"transcript_path": testTranscriptRef,
		"tool_name":       "Bash",
		"tool_use_id":     "toolu_abc",
		"tool_input": map[string]any{
			"command": "cat file.txt",
		},
	}
	blob, _ := json.Marshal(prov)

	result, err := RedactForUpload(blob, "step_provenance", testRepoRoot)
	if err != nil {
		t.Fatal(err)
	}

	// transcript_path should be dropped.
	if strings.Contains(string(result), "transcript_path") {
		t.Error("expected transcript_path to be dropped")
	}

	// cwd should be normalized (not absolute).
	if strings.Contains(string(result), testRepoRoot) {
		t.Error("expected absolute cwd to be normalized")
	}

	// tool_name should be preserved.
	if !strings.Contains(string(result), "Bash") {
		t.Error("expected tool_name to be preserved")
	}
}

func TestDeriveUploadHash_DeterministicForSameInput(t *testing.T) {
	blob := []byte(`{"file_path": "/workspace/repo/main.go", "content": "hello"}`)

	hash1, _, _ := DeriveUploadHash(blob, "step_provenance", testGenericRepoRoot)
	hash2, _, _ := DeriveUploadHash(blob, "step_provenance", testGenericRepoRoot)

	if hash1 != hash2 {
		t.Errorf("expected deterministic hash, got %s and %s", hash1, hash2)
	}
}

func TestRewriteBundleHashes_RewritesPromptAndSteps(t *testing.T) {
	bundle := map[string]any{
		"version": 1,
		"turn_id": "t1",
		"prompt": map[string]any{
			"event_id":  "evt_prompt",
			"blob_hash": "local_prompt_hash_abcdef12",
		},
		"steps": []map[string]any{
			{
				"event_id":        "evt_1",
				"ts":              1700000000,
				"tool_name":       "Write",
				"provenance_hash": "local_step1_hash_12345678",
			},
			{
				"event_id":        "evt_2",
				"ts":              1700000001,
				"tool_name":       "Edit",
				"provenance_hash": "local_step2_hash_87654321",
			},
			{
				"event_id":  "evt_3",
				"ts":        1700000002,
				"tool_name": "Bash",
				// No provenance_hash - should be left alone.
			},
		},
	}
	blob, _ := json.Marshal(bundle)

	hashMap := map[string]string{
		"local_prompt_hash_abcdef12": "upload_prompt_hash_99999999",
		"local_step1_hash_12345678":  "upload_step1_hash_aaaaaaaa",
		"local_step2_hash_87654321":  "upload_step2_hash_bbbbbbbb",
	}

	result := RewriteBundleHashes(blob, hashMap)

	// Verify the rewritten bundle references upload hashes.
	if strings.Contains(string(result), "local_prompt_hash") {
		t.Error("expected local prompt hash to be replaced")
	}
	if !strings.Contains(string(result), "upload_prompt_hash_99999999") {
		t.Error("expected upload prompt hash in output")
	}
	if strings.Contains(string(result), "local_step1_hash") {
		t.Error("expected local step1 hash to be replaced")
	}
	if !strings.Contains(string(result), "upload_step1_hash_aaaaaaaa") {
		t.Error("expected upload step1 hash in output")
	}
	if !strings.Contains(string(result), "upload_step2_hash_bbbbbbbb") {
		t.Error("expected upload step2 hash in output")
	}

	// Preserved fields should survive the round-trip.
	if !strings.Contains(string(result), "evt_prompt") {
		t.Error("expected prompt event_id to be preserved")
	}
	if !strings.Contains(string(result), "evt_3") {
		t.Error("expected step without provenance_hash to be preserved")
	}
	if !strings.Contains(string(result), `"version"`) {
		t.Error("expected version field to be preserved")
	}
}

func TestRewriteBundleHashes_EmptyMapNoOp(t *testing.T) {
	blob := []byte(`{"turn_id":"t1","steps":[]}`)
	result := RewriteBundleHashes(blob, map[string]string{})
	if string(result) != string(blob) {
		t.Errorf("empty hash map should return original bytes, got %s", string(result))
	}
}

func TestRewriteBundleHashes_NoMatchingHashes(t *testing.T) {
	bundle := map[string]any{
		"prompt": map[string]any{
			"event_id":  "e1",
			"blob_hash": "hash_not_in_map_12345678",
		},
		"steps": []map[string]any{},
	}
	blob, _ := json.Marshal(bundle)

	// Map has no entry for the prompt's hash.
	hashMap := map[string]string{"some_other_hash": "upload_hash"}
	result := RewriteBundleHashes(blob, hashMap)

	// Should return original bytes since no rewrites happened.
	if !strings.Contains(string(result), "hash_not_in_map_12345678") {
		t.Error("expected original hash to be preserved when not in map")
	}
}

func TestRewriteBundleHashes_PreservesExtraFields(t *testing.T) {
	bundle := map[string]any{
		"version":           1,
		"provider":          "claude_code",
		"session_id":        "sess1",
		"cwd":               "/workspace/project",
		"parent_session_id": "parent1",
		"steps":             []map[string]any{},
	}
	blob, _ := json.Marshal(bundle)

	result := RewriteBundleHashes(blob, map[string]string{"x": "y"})

	// Extra fields not in the typed struct should survive.
	for _, field := range []string{"provider", "session_id", "cwd", "parent_session_id"} {
		if !strings.Contains(string(result), field) {
			t.Errorf("expected %s to be preserved in output", field)
		}
	}
}

func TestRewriteBundleHashes_PreservesUnknownStepFields(t *testing.T) {
	// Simulates a future bundle schema that adds new step fields.
	// RewriteBundleHashes must preserve them without an update.
	bundle := map[string]any{
		"steps": []map[string]any{
			{
				"event_id":         "evt_1",
				"provenance_hash":  "local_hash_12345678",
				"future_field":     "should_survive",
				"another_new_flag": true,
				"nested_new": map[string]any{
					"deep": "value",
				},
			},
		},
	}
	blob, _ := json.Marshal(bundle)

	hashMap := map[string]string{"local_hash_12345678": "upload_hash_aaaaaaaa"}
	result := RewriteBundleHashes(blob, hashMap)

	// Hash should be rewritten.
	if !strings.Contains(string(result), "upload_hash_aaaaaaaa") {
		t.Error("expected provenance_hash to be rewritten")
	}
	// Unknown fields must survive the round-trip.
	for _, field := range []string{"future_field", "should_survive", "another_new_flag", "deep"} {
		if !strings.Contains(string(result), field) {
			t.Errorf("expected unknown field %q to be preserved", field)
		}
	}
}

func TestDeriveUploadHash_DiffersWhenRedactionChangesBytes(t *testing.T) {
	// Step provenance with an absolute path: redaction normalizes it.
	prov := map[string]any{
		"tool_input": map[string]any{
			"file_path": testGenericRepoMainGo,
			"content":   "hello",
		},
	}
	blob, _ := json.Marshal(prov)

	// Upload hash computed with repo root (path gets normalized).
	uploadHash, _, _ := DeriveUploadHash(blob, "step_provenance", testGenericRepoRoot)

	// Raw CAS hash is sha256 of the original (un-redacted) bytes.
	rawDigest := sha256.Sum256(blob)
	rawHash := hex.EncodeToString(rawDigest[:])

	if uploadHash == rawHash {
		t.Error("upload hash should differ from raw CAS hash when redaction changes the bytes")
	}
}

func TestExtractPromptHashFromBytes(t *testing.T) {
	bundle := map[string]any{
		"prompt": map[string]any{
			"event_id":  "e1",
			"blob_hash": "abc12345",
		},
	}
	blob, _ := json.Marshal(bundle)

	got := extractPromptHashFromBytes(blob)
	if got != "abc12345" {
		t.Errorf("extractPromptHashFromBytes = %q, want abc12345", got)
	}
}

func TestExtractPromptHashFromBytes_NoPrompt(t *testing.T) {
	blob := []byte(`{"steps":[]}`)
	got := extractPromptHashFromBytes(blob)
	if got != "" {
		t.Errorf("expected empty string for bundle without prompt, got %q", got)
	}
}

func TestExtractStepProvenanceHashes(t *testing.T) {
	bundle := map[string]any{
		"steps": []map[string]any{
			{"provenance_hash": "hash_a_12345678"},
			{"provenance_hash": "hash_b_87654321"},
			{"provenance_hash": ""},                // empty - skipped
			{"provenance_hash": "hash_a_12345678"}, // duplicate - deduped
			{},                                     // no field - skipped
		},
	}
	blob, _ := json.Marshal(bundle)

	got := extractStepProvenanceHashes(blob)
	if len(got) != 2 {
		t.Fatalf("expected 2 unique hashes, got %d: %v", len(got), got)
	}
	if got[0] != "hash_a_12345678" || got[1] != "hash_b_87654321" {
		t.Errorf("unexpected hashes: %v", got)
	}
}

func TestRedactPrompt_FailsClosedOnRedactError(t *testing.T) {
	cleanup := redact.ForceInitError(errors.New("forced detector failure"))
	defer cleanup()

	_, err := RedactForUpload([]byte("ordinary prompt text"), "prompt", "/repo")
	if err == nil {
		t.Fatal("expected error when redactor fails, got nil")
	}
}

func TestRedactBundle_FailsClosedOnInvalidJSON(t *testing.T) {
	_, err := RedactForUpload([]byte("not json {{"), "bundle", testRepoRoot)
	if err == nil {
		t.Fatal("expected error on malformed bundle JSON, got nil")
	}
}

func TestRedactStepProvenance_FailsClosedOnInvalidJSON(t *testing.T) {
	_, err := RedactForUpload([]byte("not json {{"), "step_provenance", testRepoRoot)
	if err == nil {
		t.Fatal("expected error on malformed step_provenance JSON, got nil")
	}
}

func TestRedactStepProvenance_FailsClosedOnTopLevelRedactError(t *testing.T) {
	prov := map[string]any{
		"command": "echo hello",
	}
	blob, _ := json.Marshal(prov)

	cleanup := redact.ForceInitError(errors.New("forced detector failure"))
	defer cleanup()

	_, err := RedactForUpload(blob, "step_provenance", testRepoRoot)
	if err == nil {
		t.Fatal("expected error when redactor fails on top-level command, got nil")
	}
}

func TestRedactToolFields_FailsClosedOnNestedRedactError(t *testing.T) {
	prov := map[string]any{
		"tool_input": map[string]any{
			"new_string": "some content",
		},
	}
	blob, _ := json.Marshal(prov)

	cleanup := redact.ForceInitError(errors.New("forced detector failure"))
	defer cleanup()

	_, err := RedactForUpload(blob, "step_provenance", testRepoRoot)
	if err == nil {
		t.Fatal("expected error when redactor fails on nested new_string, got nil")
	}
}

// fakeSlackWebhook returns a string gitleaks reliably matches.
// Built from parts so this file does not itself trip secret scanners.
func fakeSlackWebhook() string {
	return "https://hooks.slack.com/" +
		"services/" +
		"T00000000/" +
		"B00000000/" +
		"XXXXXXXXXXXXXXXXXXXXXXXX"
}

func TestRedactToolFields_ScalarStringRedacted(t *testing.T) {
	secret := fakeSlackWebhook()
	// tool_response stored as a raw JSON string with a fake secret.
	prov := map[string]any{
		"tool_response": "Posted to " + secret + " successfully.",
	}
	blob, _ := json.Marshal(prov)

	result, err := RedactForUpload(blob, "step_provenance", testRepoRoot)
	if err != nil {
		t.Fatalf("scalar string tool_response should not fail: %v", err)
	}
	if strings.Contains(string(result), secret) {
		t.Errorf("expected secret to be redacted in scalar string tool_response, got: %s", string(result))
	}
}

func TestRedactToolFields_ArrayRecurses(t *testing.T) {
	secret := fakeSlackWebhook()
	// tool_response stored as an array mixing strings and objects.
	prov := map[string]any{
		"tool_response": []any{
			"first contains " + secret + " value",
			map[string]any{
				"stdout": "second contains " + secret + " too",
			},
			42,
			nil,
		},
	}
	blob, _ := json.Marshal(prov)

	result, err := RedactForUpload(blob, "step_provenance", testRepoRoot)
	if err != nil {
		t.Fatalf("array tool_response should not fail: %v", err)
	}
	if strings.Contains(string(result), secret) {
		t.Errorf("expected all secret occurrences in the array to be redacted, got: %s", string(result))
	}
	if !strings.Contains(string(result), "42") {
		t.Errorf("expected number element to be preserved, got: %s", string(result))
	}
	if !strings.Contains(string(result), "null") {
		t.Errorf("expected null element to be preserved, got: %s", string(result))
	}
}

func TestRedactToolFields_NumberBoolNullPassthrough(t *testing.T) {
	cases := []struct {
		name  string
		value any
	}{
		{"number", 42},
		{"bool", true},
		{"null", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov := map[string]any{
				"tool_response": tc.value,
			}
			blob, _ := json.Marshal(prov)

			result, err := RedactForUpload(blob, "step_provenance", testRepoRoot)
			if err != nil {
				t.Fatalf("scalar %s tool_response should not fail: %v", tc.name, err)
			}
			var out map[string]json.RawMessage
			if err := json.Unmarshal(result, &out); err != nil {
				t.Fatalf("unmarshal result: %v", err)
			}
			origJSON, _ := json.Marshal(tc.value)
			if string(out["tool_response"]) != string(origJSON) {
				t.Errorf("tool_response = %s, want %s", string(out["tool_response"]), string(origJSON))
			}
		})
	}
}

func TestRedactToolFields_FailsClosedOnMalformedJSON(t *testing.T) {
	blob := []byte(`{"tool_response": not_valid_json}`)

	_, err := RedactForUpload(blob, "step_provenance", testRepoRoot)
	if err == nil {
		t.Fatal("expected error on malformed tool_response, got nil")
	}
}

func TestRedactForUpload_UnknownKindFailsClosed(t *testing.T) {
	_, err := RedactForUpload([]byte(`{"x":1}`), "future_kind", "/repo")
	if err == nil {
		t.Fatal("expected error on unknown blob kind, got nil")
	}
	if !strings.Contains(err.Error(), "unknown blob kind") {
		t.Errorf("expected error message to mention unknown blob kind, got: %v", err)
	}
}

func TestNormalizePath_RepoRelative(t *testing.T) {
	cases := []struct {
		path     string
		repoRoot string
		want     string
	}{
		{filepath.Join(testGenericRepoRoot, "src", "main.go"), testGenericRepoRoot, "src/main.go"},
		{testGenericRepoRoot, testGenericRepoRoot, "."},
		{"/other/path/file.go", testGenericRepoRoot, ""},                                   // absolute outside repo - dropped
		{"relative/path.go", testGenericRepoRoot, "relative/path.go"},                      // already relative inside repo
		{"../secret.txt", testGenericRepoRoot, ""},                                         // relative escape - dropped
		{"../../other-repo/file.go", testGenericRepoRoot, ""},                              // relative escape - dropped
		{filepath.Join("src", "..", "src", "main.go"), testGenericRepoRoot, "src/main.go"}, // cleaned but still inside repo
		{"", testGenericRepoRoot, ""},
	}
	for _, tc := range cases {
		got := normalizePath(tc.path, tc.repoRoot)
		if got != tc.want {
			t.Errorf("normalizePath(%q, %q) = %q, want %q", tc.path, tc.repoRoot, got, tc.want)
		}
	}
}

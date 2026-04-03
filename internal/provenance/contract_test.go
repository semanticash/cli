package provenance

// Contract tests that keep CLI packaging and remote upload artifacts aligned.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
)

// uploadedBundleJSON mirrors the bundle shape expected by the remote reader.
type uploadedBundleJSON struct {
	Version int    `json:"version"`
	CWD     string `json:"cwd,omitempty"`
	Prompt  *struct {
		EventID  string `json:"event_id"`
		BlobHash string `json:"blob_hash"`
	} `json:"prompt"`
	Steps []struct {
		EventID        string   `json:"event_id"`
		ToolName       string   `json:"tool_name"`
		ToolUseID      string   `json:"tool_use_id"`
		ProvenanceHash string   `json:"provenance_hash"`
		Summary        *string  `json:"summary"`
		FilePaths      []string `json:"file_paths"`
	} `json:"steps"`
}

// remoteObjectKey mirrors the remote object key layout.
func remoteObjectKey(tenantID, kind, hash string) string {
	return fmt.Sprintf("%s/provenance/%s/%s/%s", tenantID, kind, hash[:2], hash)
}

func TestContractEndToEnd_RewrittenBundleAcceptedByComplete(t *testing.T) {
	repoRoot := "/workspace/myrepo"

	// --- Simulate what package.go produces ---

	// Raw prompt blob (what gets stored in local CAS).
	rawPrompt := []byte(`Deploy the service to staging with OPENAI_KEY=sk-abc123.`)

	// Raw step provenance blobs (local CAS).
	rawStepWrite := mustJSON(t, map[string]any{
		"tool_input": map[string]any{
			"file_path": repoRoot + "/src/handler.go",
			"content":   "package main\nvar secret = \"REDACT_ME_s3cr3t\"\n",
		},
	})
	rawStepEdit := mustJSON(t, map[string]any{
		"tool_input": map[string]any{
			"file_path":  repoRoot + "/src/config.go",
			"old_string": "v1",
			"new_string": "v2",
		},
	})

	// Local CAS hashes (sha256 of raw bytes).
	localPromptHash := sha256Hex(rawPrompt)
	localStepWriteHash := sha256Hex(rawStepWrite)
	localStepEditHash := sha256Hex(rawStepEdit)

	// Raw bundle built by package.go: embeds local CAS hashes.
	rawBundle := mustJSON(t, map[string]any{
		"version":    1,
		"provider":   "claude_code",
		"session_id": "sess_abc",
		"turn_id":    "turn_001",
		"cwd":        repoRoot,
		"prompt": map[string]any{
			"event_id":  "evt_prompt",
			"blob_hash": localPromptHash,
		},
		"steps": []map[string]any{
			{
				"event_id":        "evt_write",
				"ts":              1700000001,
				"tool_name":       "Write",
				"tool_use_id":     "toolu_w1",
				"provenance_hash": localStepWriteHash,
				"file_paths":      []string{repoRoot + "/src/handler.go"},
			},
			{
				"event_id":        "evt_edit",
				"ts":              1700000002,
				"tool_name":       "Edit",
				"tool_use_id":     "toolu_e1",
				"provenance_hash": localStepEditHash,
				"file_paths":      []string{repoRoot + "/src/config.go"},
			},
		},
	})

	// --- Simulate what sync.go does ---

	// Step 1: Redact prompt and step provenance blobs, get upload hashes.
	promptUploadHash, promptRedacted, err := DeriveUploadHash(rawPrompt, "prompt", repoRoot)
	if err != nil {
		t.Fatalf("DeriveUploadHash(prompt): %v", err)
	}
	stepWriteUploadHash, stepWriteRedacted, err := DeriveUploadHash(rawStepWrite, "step_provenance", repoRoot)
	if err != nil {
		t.Fatalf("DeriveUploadHash(step_write): %v", err)
	}
	stepEditUploadHash, stepEditRedacted, err := DeriveUploadHash(rawStepEdit, "step_provenance", repoRoot)
	if err != nil {
		t.Fatalf("DeriveUploadHash(step_edit): %v", err)
	}

	// At least one blob must have differing local/upload hashes, proving
	// the rewrite is load-bearing. Step provenance with absolute paths
	// will always differ due to path normalization.
	anyDiffers := (localPromptHash != promptUploadHash) ||
		(localStepWriteHash != stepWriteUploadHash) ||
		(localStepEditHash != stepEditUploadHash)
	if !anyDiffers {
		t.Fatal("no blob hashes changed after redaction - rewrite would be untested")
	}
	// Step provenance with absolute file_path must differ.
	if localStepWriteHash == stepWriteUploadHash {
		t.Error("step_write: expected hash to differ after path normalization")
	}

	// Step 2: Build hash map and rewrite bundle.
	hashMap := map[string]string{
		localPromptHash:    promptUploadHash,
		localStepWriteHash: stepWriteUploadHash,
		localStepEditHash:  stepEditUploadHash,
	}
	rewrittenBundle := RewriteBundleHashes(rawBundle, hashMap)

	// Step 3: Redact the rewritten bundle.
	bundleUploadHash, bundleRedacted, err := DeriveUploadHash(rewrittenBundle, "bundle", repoRoot)
	if err != nil {
		t.Fatalf("DeriveUploadHash(bundle): %v", err)
	}

	// Build the envelope (what sync.go sends to POST /complete).
	type envelopeObject struct {
		Kind      string `json:"kind"`
		Hash      string `json:"hash"`
		SizeBytes int    `json:"size_bytes"`
	}
	objects := []envelopeObject{
		{Kind: "prompt", Hash: promptUploadHash, SizeBytes: len(promptRedacted)},
		{Kind: "step_provenance", Hash: stepWriteUploadHash, SizeBytes: len(stepWriteRedacted)},
		{Kind: "step_provenance", Hash: stepEditUploadHash, SizeBytes: len(stepEditRedacted)},
		{Kind: "bundle", Hash: bundleUploadHash, SizeBytes: len(bundleRedacted)},
	}

	// --- Simulate what the remote reader does ---

	// Parse the uploaded bundle bytes the same way the remote reader would.
	var uploadedBundle uploadedBundleJSON
	if err := json.Unmarshal(bundleRedacted, &uploadedBundle); err != nil {
		t.Fatalf("remote reader cannot parse rewritten+redacted bundle: %v", err)
	}

	// Build object lookup (same as CompleteProvenance line 167-174).
	objectsByKindHash := make(map[string]bool)
	for _, obj := range objects {
		objectsByKindHash[obj.Kind+":"+obj.Hash] = true
	}

	// Validate that the prompt hash is present in the uploaded object list.
	if uploadedBundle.Prompt != nil && uploadedBundle.Prompt.BlobHash != "" {
		key := "prompt:" + uploadedBundle.Prompt.BlobHash
		if !objectsByKindHash[key] {
			t.Errorf("bundle prompt.blob_hash %q not found in objects list",
				uploadedBundle.Prompt.BlobHash)
		}
	}

	// Validate that each step provenance hash is present in the uploaded object list.
	for _, step := range uploadedBundle.Steps {
		if step.ProvenanceHash == "" {
			continue
		}
		key := "step_provenance:" + step.ProvenanceHash
		if !objectsByKindHash[key] {
			t.Errorf("bundle step %s provenance_hash %q not found in objects list",
				step.EventID, step.ProvenanceHash)
		}
	}

	// --- Simulate fetching step blobs by uploaded hash ---

	tenantID := "tenant-test-uuid"

	// Simulate a remote object store keyed by upload hash.
	remoteStore := map[string][]byte{
		remoteObjectKey(tenantID, "prompt", promptUploadHash):             promptRedacted,
		remoteObjectKey(tenantID, "step_provenance", stepWriteUploadHash): stepWriteRedacted,
		remoteObjectKey(tenantID, "step_provenance", stepEditUploadHash):  stepEditRedacted,
		remoteObjectKey(tenantID, "bundle", bundleUploadHash):             bundleRedacted,
	}

	// For each step, verify the remote key resolves to the correct blob.
	for _, step := range uploadedBundle.Steps {
		if step.ProvenanceHash == "" {
			continue
		}
		remoteKey := remoteObjectKey(tenantID, "step_provenance", step.ProvenanceHash)
		blob, ok := remoteStore[remoteKey]
		if !ok {
			t.Errorf("remote key %q for step %s not found in store", remoteKey, step.EventID)
			continue
		}

		// The uploaded step blob should still parse as provenance JSON.
		var prov map[string]json.RawMessage
		if err := json.Unmarshal(blob, &prov); err != nil {
			t.Errorf("step %s: cannot parse provenance blob fetched via remote key: %v", step.EventID, err)
			continue
		}

		// Verify the blob contains expected tool_input.
		if _, ok := prov["tool_input"]; !ok {
			t.Errorf("step %s: provenance blob missing tool_input", step.EventID)
		}
	}

	// Verify bundle path normalization: cwd should not contain the repo root.
	if uploadedBundle.CWD == repoRoot {
		t.Error("bundle cwd should be normalized to repo-relative, not absolute")
	}

	// Verify step file_paths are repo-relative.
	for _, step := range uploadedBundle.Steps {
		for _, fp := range step.FilePaths {
			if len(fp) > 0 && fp[0] == '/' {
				t.Errorf("step %s: file_path %q should be repo-relative, not absolute", step.EventID, fp)
			}
		}
	}
}

// TestContractEndToEnd_MissingBlobInBundleDetectedLocally proves that
// the sync pipeline marks a manifest as failed (rather than sending a
// partial envelope) when a blob referenced by the bundle is missing.
// This is tested at the hash-extraction level since buildSyncResult
// requires a full DB/blob store setup.
func TestContractEndToEnd_MissingBlobDetectedByExtraction(t *testing.T) {
	// Bundle references a prompt and two step provenance blobs.
	bundle := mustJSON(t, map[string]any{
		"prompt": map[string]any{
			"event_id":  "evt_p",
			"blob_hash": "prompt_hash_12345678",
		},
		"steps": []map[string]any{
			{"event_id": "evt_1", "provenance_hash": "step_hash_aaaaaaaa"},
			{"event_id": "evt_2", "provenance_hash": "step_hash_bbbbbbbb"},
		},
	})

	// extractPromptHashFromBytes finds the reference.
	promptHash := extractPromptHashFromBytes(bundle)
	if promptHash != "prompt_hash_12345678" {
		t.Fatalf("expected prompt hash extraction, got %q", promptHash)
	}

	// extractStepProvenanceHashes finds all references.
	stepHashes := extractStepProvenanceHashes(bundle)
	if len(stepHashes) != 2 {
		t.Fatalf("expected 2 step hashes, got %d", len(stepHashes))
	}

	// In the real sync flow, if loadAndRedact fails for any of these hashes,
	// the manifest is marked failed. We verify that the extraction correctly
	// identifies ALL hashes that must be present for the envelope to be valid.
	requiredHashes := make(map[string]bool)
	if promptHash != "" {
		requiredHashes[promptHash] = true
	}
	for _, h := range stepHashes {
		requiredHashes[h] = true
	}

	// Simulate: only step_hash_aaaaaaaa is available locally.
	availableLocally := map[string]bool{
		"step_hash_aaaaaaaa": true,
	}

	missing := 0
	for h := range requiredHashes {
		if !availableLocally[h] {
			missing++
		}
	}
	if missing != 2 {
		t.Errorf("expected 2 missing blobs (prompt + step_hash_bb), got %d", missing)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return data
}

func sha256Hex(b []byte) string {
	d := sha256.Sum256(b)
	return hex.EncodeToString(d[:])
}

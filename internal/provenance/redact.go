package provenance

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/redact"
)

// UploadTransformVersion tracks the current redaction/normalization rules.
// Increment when the transform output changes for the same input.
const UploadTransformVersion = 1

// RedactForUpload transforms a raw CAS blob into an upload-safe artifact.
// Applies path normalization and secret redaction based on the object kind.
// The output is deterministic for a given transform version.
//
// Invalid JSON, unknown kinds, and redactor failures return an error
// without upload bytes.
func RedactForUpload(blob []byte, kind string, repoRoot string) ([]byte, error) {
	switch kind {
	case "prompt":
		return redactPrompt(blob)
	case "bundle":
		return redactBundle(blob, repoRoot)
	case "step_provenance":
		return redactStepProvenance(blob, repoRoot)
	default:
		slog.Warn("provenance: redaction failed", "kind", kind, "reason", "unknown_kind")
		return nil, fmt.Errorf("unknown blob kind for upload: %q", kind)
	}
}

// DeriveUploadHash redacts a blob and computes the content hash of the result.
// Returns both the upload hash (for object naming and dedup) and the redacted bytes.
func DeriveUploadHash(blob []byte, kind string, repoRoot string) (uploadHash string, redactedBlob []byte, err error) {
	redacted, err := RedactForUpload(blob, kind, repoRoot)
	if err != nil {
		return "", nil, err
	}
	h := sha256.Sum256(redacted)
	return hex.EncodeToString(h[:]), redacted, nil
}

// redactPrompt redacts secrets from prompt text.
func redactPrompt(blob []byte) ([]byte, error) {
	redacted, err := redact.Bytes(blob)
	if err != nil {
		slog.Warn("provenance: redaction failed", "kind", "prompt", "reason", "apply", "err", err)
		return nil, fmt.Errorf("redact prompt: %w", err)
	}
	return redacted, nil
}

// redactBundle normalizes paths in the bundle JSON.
func redactBundle(blob []byte, repoRoot string) ([]byte, error) {
	var bundle map[string]json.RawMessage
	if err := json.Unmarshal(blob, &bundle); err != nil {
		slog.Warn("provenance: redaction failed", "kind", "bundle", "reason", "unmarshal", "err", err)
		return nil, fmt.Errorf("redact bundle: unmarshal: %w", err)
	}

	// Normalize cwd to repo-relative.
	if cwdRaw, ok := bundle["cwd"]; ok {
		var cwd string
		if json.Unmarshal(cwdRaw, &cwd) == nil && cwd != "" {
			cwd = normalizePath(cwd, repoRoot)
			normalized, _ := json.Marshal(cwd)
			bundle["cwd"] = normalized
		}
	}

	// Drop transcript_ref (local-only path).
	delete(bundle, "transcript_ref")

	// Normalize file_paths in steps.
	if stepsRaw, ok := bundle["steps"]; ok {
		var steps []map[string]json.RawMessage
		if json.Unmarshal(stepsRaw, &steps) == nil {
			for i := range steps {
				if fpRaw, ok := steps[i]["file_paths"]; ok {
					var paths []string
					if json.Unmarshal(fpRaw, &paths) == nil {
						for j := range paths {
							paths[j] = normalizePath(paths[j], repoRoot)
						}
						normalized, _ := json.Marshal(paths)
						steps[i]["file_paths"] = normalized
					}
				}
			}
			normalizedSteps, _ := json.Marshal(steps)
			bundle["steps"] = normalizedSteps
		}
	}

	return json.Marshal(bundle)
}

// redactStepProvenance normalizes paths and redacts secrets in a step
// provenance blob. Handles both nested fields (tool_input/tool_response)
// and top-level fields (cwd, transcript_path, file_path) that some
// providers include.
func redactStepProvenance(blob []byte, repoRoot string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(blob, &obj); err != nil {
		slog.Warn("provenance: redaction failed", "kind", "step_provenance", "reason", "unmarshal", "err", err)
		return nil, fmt.Errorf("redact step_provenance: unmarshal: %w", err)
	}

	// Drop or normalize top-level local-only fields.
	for _, key := range []string{"transcript_path", "transcript_ref"} {
		delete(obj, key)
	}

	// Normalize top-level path fields.
	for _, key := range []string{"cwd", "file_path", "filePath", "path"} {
		if v, ok := obj[key]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && s != "" {
				s = normalizePath(s, repoRoot)
				normalized, _ := json.Marshal(s)
				obj[key] = normalized
			}
		}
	}

	// Redact top-level string fields that may contain secrets.
	for _, key := range []string{"command", "stdout", "stderr"} {
		if v, ok := obj[key]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && s != "" {
				redacted, err := redact.String(s)
				if err != nil {
					slog.Warn("provenance: redaction failed", "kind", "step_provenance", "reason", "apply", "field", key, "err", err)
					return nil, fmt.Errorf("redact step_provenance: top-level %s: %w", key, err)
				}
				r, _ := json.Marshal(redacted)
				obj[key] = r
			}
		}
	}

	// Normalize and redact nested tool_input fields.
	if inputRaw, ok := obj["tool_input"]; ok {
		redacted, err := redactToolFields(inputRaw, repoRoot)
		if err != nil {
			return nil, fmt.Errorf("redact step_provenance: tool_input: %w", err)
		}
		obj["tool_input"] = redacted
	}

	// Normalize and redact nested tool_response fields.
	if respRaw, ok := obj["tool_response"]; ok {
		redacted, err := redactToolFields(respRaw, repoRoot)
		if err != nil {
			return nil, fmt.Errorf("redact step_provenance: tool_response: %w", err)
		}
		obj["tool_response"] = redacted
	}

	// Canonical multi-file shape. Each entry carries file-edit content
	// fields and an entry-local path.
	if filesRaw, ok := obj["files"]; ok {
		redacted, err := redactFilesArray(filesRaw, repoRoot)
		if err != nil {
			return nil, fmt.Errorf("redact step_provenance: files: %w", err)
		}
		obj["files"] = redacted
	}

	return json.Marshal(obj)
}

// redactFilesArray walks the canonical files[] wrapper and runs
// redactToolFields on every element. Canonical entries are objects,
// but non-object entries are scanned too so malformed producer output
// cannot bypass redaction.
func redactFilesArray(raw json.RawMessage, repoRoot string) (json.RawMessage, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		slog.Warn("provenance: redaction failed", "kind", "step_provenance", "reason", "unmarshal", "field", "files", "err", err)
		return nil, fmt.Errorf("files: unmarshal: %w", err)
	}
	for i, entry := range entries {
		redacted, err := redactToolFields(entry, repoRoot)
		if err != nil {
			return nil, fmt.Errorf("files[%d]: %w", i, err)
		}
		entries[i] = redacted
	}
	out, err := json.Marshal(entries)
	if err != nil {
		return nil, fmt.Errorf("files: marshal: %w", err)
	}
	return out, nil
}

// redactToolFields normalizes paths and redacts secret content in a
// tool_input or tool_response JSON value. Providers may store these
// fields as objects, arrays, or scalar values.
func redactToolFields(raw json.RawMessage, repoRoot string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return raw, nil
	}

	switch trimmed[0] {
	case '{':
		return redactToolFieldsObject(raw, repoRoot)
	case '[':
		return redactToolFieldsArray(raw, repoRoot)
	case '"':
		return redactToolFieldsString(raw)
	default:
		// Numbers, booleans, and null do not need redaction, but still
		// need to parse as valid JSON.
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			slog.Warn("provenance: redaction failed", "kind", "step_provenance", "reason", "unmarshal", "field", "tool_fields", "err", err)
			return nil, fmt.Errorf("tool_fields: unmarshal: %w", err)
		}
		return raw, nil
	}
}

func redactToolFieldsObject(raw json.RawMessage, repoRoot string) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		slog.Warn("provenance: redaction failed", "kind", "step_provenance", "reason", "unmarshal", "field", "tool_fields_object", "err", err)
		return nil, fmt.Errorf("tool_fields object: unmarshal: %w", err)
	}

	// Path fields: normalize to repo-relative.
	for _, key := range []string{"file_path", "path", "filePath"} {
		if v, ok := fields[key]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && s != "" {
				s = normalizePath(s, repoRoot)
				normalized, _ := json.Marshal(s)
				fields[key] = normalized
			}
		}
	}

	// Content fields: redact secrets in nested tool payloads and
	// canonical files[] entries.
	for _, key := range []string{
		"content", "file_text", "new_string", "old_string",
		"new_str", "old_str", "newString", "oldString",
		"new_text", "old_text",
		"command", "stdout", "stderr",
		"originalFile", "newContent", "originalContent",
	} {
		if v, ok := fields[key]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && s != "" {
				redacted, err := redact.String(s)
				if err != nil {
					slog.Warn("provenance: redaction failed", "kind", "step_provenance", "reason", "apply", "field", key, "err", err)
					return nil, fmt.Errorf("tool_fields object: %s: %w", key, err)
				}
				r, _ := json.Marshal(redacted)
				fields[key] = r
			}
		}
	}

	result, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("tool_fields object: marshal: %w", err)
	}
	return result, nil
}

func redactToolFieldsArray(raw json.RawMessage, repoRoot string) (json.RawMessage, error) {
	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil {
		slog.Warn("provenance: redaction failed", "kind", "step_provenance", "reason", "unmarshal", "field", "tool_fields_array", "err", err)
		return nil, fmt.Errorf("tool_fields array: unmarshal: %w", err)
	}
	for i, el := range elements {
		redacted, err := redactToolFields(el, repoRoot)
		if err != nil {
			return nil, fmt.Errorf("tool_fields array[%d]: %w", i, err)
		}
		elements[i] = redacted
	}
	result, err := json.Marshal(elements)
	if err != nil {
		return nil, fmt.Errorf("tool_fields array: marshal: %w", err)
	}
	return result, nil
}

func redactToolFieldsString(raw json.RawMessage) (json.RawMessage, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		slog.Warn("provenance: redaction failed", "kind", "step_provenance", "reason", "unmarshal", "field", "tool_fields_string", "err", err)
		return nil, fmt.Errorf("tool_fields string: unmarshal: %w", err)
	}
	if s == "" {
		return raw, nil
	}
	redacted, err := redact.String(s)
	if err != nil {
		slog.Warn("provenance: redaction failed", "kind", "step_provenance", "reason", "apply", "field", "tool_fields_string", "err", err)
		return nil, fmt.Errorf("tool_fields string: %w", err)
	}
	out, err := json.Marshal(redacted)
	if err != nil {
		return nil, fmt.Errorf("tool_fields string: marshal: %w", err)
	}
	return out, nil
}

// RewriteBundleHashes replaces local CAS hashes embedded in a bundle blob
// with their corresponding upload hashes. The bundle stores prompt.blob_hash
// and steps[].provenance_hash as local CAS hashes at packaging time, but
// uploaded objects use their redacted upload hashes. This rewrite happens
// before bundle redaction so the uploaded bundle references the same hashes
// as the uploaded objects.
//
// Uses generic map surgery (not a typed struct) so new step fields added
// later are preserved automatically without updating this function.
func RewriteBundleHashes(bundleBytes []byte, hashMap map[string]string) []byte {
	if len(hashMap) == 0 {
		return bundleBytes
	}

	var generic map[string]json.RawMessage
	if err := json.Unmarshal(bundleBytes, &generic); err != nil {
		return bundleBytes
	}

	changed := false

	// Rewrite prompt.blob_hash.
	if promptRaw, ok := generic["prompt"]; ok {
		var prompt map[string]json.RawMessage
		if json.Unmarshal(promptRaw, &prompt) == nil {
			if hashRaw, ok := prompt["blob_hash"]; ok {
				var localHash string
				if json.Unmarshal(hashRaw, &localHash) == nil && localHash != "" {
					if uploadHash, ok := hashMap[localHash]; ok {
						encoded, _ := json.Marshal(uploadHash)
						prompt["blob_hash"] = encoded
						rewritten, _ := json.Marshal(prompt)
						generic["prompt"] = rewritten
						changed = true
					}
				}
			}
		}
	}

	// Rewrite steps[].provenance_hash.
	if stepsRaw, ok := generic["steps"]; ok {
		var steps []map[string]json.RawMessage
		if json.Unmarshal(stepsRaw, &steps) == nil {
			for i := range steps {
				hashRaw, ok := steps[i]["provenance_hash"]
				if !ok {
					continue
				}
				var localHash string
				if json.Unmarshal(hashRaw, &localHash) != nil || localHash == "" {
					continue
				}
				if uploadHash, ok := hashMap[localHash]; ok {
					encoded, _ := json.Marshal(uploadHash)
					steps[i]["provenance_hash"] = encoded
					changed = true
				}
			}
			if changed {
				rewritten, _ := json.Marshal(steps)
				generic["steps"] = rewritten
			}
		}
	}

	if !changed {
		return bundleBytes
	}

	result, err := json.Marshal(generic)
	if err != nil {
		return bundleBytes
	}
	return result
}

// normalizePath converts a path to repo-relative.
// Returns empty string for any path (absolute or relative) that
// escapes the repo root, so machine-specific or out-of-repo paths
// never leak into uploaded artifacts.
func normalizePath(p string, repoRoot string) string {
	if repoRoot == "" || p == "" {
		return p
	}
	if platform.LooksAbsolutePath(p) {
		// Clean both to native separators so filepath.Rel works across
		// POSIX and Windows path styles.
		rel, err := filepath.Rel(filepath.Clean(repoRoot), filepath.Clean(p))
		if err != nil {
			return ""
		}
		p = rel
	}
	cleaned := filepath.ToSlash(filepath.Clean(p))
	if strings.HasPrefix(cleaned, "..") {
		return ""
	}
	return cleaned
}

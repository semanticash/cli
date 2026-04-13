package provenance

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
func RedactForUpload(blob []byte, kind string, repoRoot string) ([]byte, error) {
	switch kind {
	case "prompt":
		return redactPrompt(blob)
	case "bundle":
		return redactBundle(blob, repoRoot)
	case "step_provenance":
		return redactStepProvenance(blob, repoRoot)
	default:
		return blob, nil
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
		return blob, nil // fail open - return original
	}
	return redacted, nil
}

// redactBundle normalizes paths in the bundle JSON.
func redactBundle(blob []byte, repoRoot string) ([]byte, error) {
	var bundle map[string]json.RawMessage
	if err := json.Unmarshal(blob, &bundle); err != nil {
		return blob, nil
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
		return blob, nil
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
				if err == nil {
					r, _ := json.Marshal(redacted)
					obj[key] = r
				}
			}
		}
	}

	// Normalize and redact nested tool_input fields.
	if inputRaw, ok := obj["tool_input"]; ok {
		redacted := redactToolFields(inputRaw, repoRoot)
		obj["tool_input"] = redacted
	}

	// Normalize and redact nested tool_response fields.
	if respRaw, ok := obj["tool_response"]; ok {
		redacted := redactToolFields(respRaw, repoRoot)
		obj["tool_response"] = redacted
	}

	return json.Marshal(obj)
}

// redactToolFields normalizes paths and redacts secret content in a tool
// input or response JSON object.
func redactToolFields(raw json.RawMessage, repoRoot string) json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return raw
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

	// Content fields: redact secrets.
	for _, key := range []string{
		"content", "file_text", "new_string", "old_string",
		"new_str", "old_str", "newString", "oldString",
		"command", "stdout", "stderr",
		"originalFile", "newContent", "originalContent",
	} {
		if v, ok := fields[key]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && s != "" {
				redacted, err := redact.String(s)
				if err == nil {
					r, _ := json.Marshal(redacted)
					fields[key] = r
				}
			}
		}
	}

	result, _ := json.Marshal(fields)
	return result
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

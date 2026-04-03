package provenance

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"strings"
)

// checkGitIgnored tests which paths are ignored by Git ignore rules
// (.gitignore, .git/info/exclude, global ignore config). Uses a single
// `git check-ignore --stdin -z` call with all paths batched via stdin.
// Fail-open: returns an empty set and logs a warning on any git error.
func checkGitIgnored(ctx context.Context, repoRoot string, paths []string) map[string]bool {
	if len(paths) == 0 {
		return nil
	}

	// Build NUL-delimited input.
	var stdin bytes.Buffer
	for _, p := range paths {
		stdin.WriteString(p)
		stdin.WriteByte(0)
	}

	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "check-ignore", "--stdin", "-z")
	cmd.Stdin = &stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	// Exit code 0: at least one match. Exit code 1: no matches (not a failure).
	// Anything else (128+) is a real error.
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 1 {
				// No matches -- all paths are visible.
				return nil
			}
		}
		slog.Warn("provenance: git check-ignore failed, keeping all steps",
			"repo", repoRoot, "err", err, "stderr", stderr.String())
		return nil
	}

	// Parse NUL-delimited output. Preserve raw path strings exactly;
	// only skip empty segments between consecutive NUL bytes or trailing NUL.
	ignored := make(map[string]bool)
	for _, p := range strings.Split(stdout.String(), "\x00") {
		if p != "" {
			ignored[p] = true
		}
	}
	return ignored
}

// extractPrimaryFile extracts the primary file path from a provenance blob
// generically across providers. Checks tool_input.file_path, tool_input.path,
// tool_input.filePath, and top-level file_path/path/filePath. Returns the
// first non-empty value found, or empty string if none.
func extractPrimaryFile(blob []byte) string {
	if len(blob) == 0 {
		return ""
	}

	var obj struct {
		FilePath  string `json:"file_path"`
		Path      string `json:"path"`
		FilePathC string `json:"filePath"`
		ToolInput struct {
			FilePath  string `json:"file_path"`
			Path      string `json:"path"`
			FilePathC string `json:"filePath"`
		} `json:"tool_input"`
	}
	if json.Unmarshal(blob, &obj) != nil {
		return ""
	}

	// Prefer tool_input fields (most providers put it there).
	if obj.ToolInput.FilePath != "" {
		return obj.ToolInput.FilePath
	}
	if obj.ToolInput.Path != "" {
		return obj.ToolInput.Path
	}
	if obj.ToolInput.FilePathC != "" {
		return obj.ToolInput.FilePathC
	}
	// Fallback to top-level fields.
	if obj.FilePath != "" {
		return obj.FilePath
	}
	if obj.Path != "" {
		return obj.Path
	}
	return obj.FilePathC
}

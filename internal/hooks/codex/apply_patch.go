package codex

import (
	"path/filepath"
	"strings"
)

// applyPatchOp identifies the kind of change one envelope section
// describes. The grammar Codex emits inside tool_input.command groups
// changes by file with one of four headers (Add/Update/Delete/Move).
type applyPatchOp int

const (
	applyPatchOpUnknown applyPatchOp = iota
	applyPatchOpAdd
	applyPatchOpUpdate
	applyPatchOpDelete
	applyPatchOpMove
)

// applyPatchFile is one file's worth of change parsed out of an
// apply_patch envelope. content captures the new-line text for
// Add/Update operations (lines that were prefixed with '+' in the
// hunk-style update body, or every line in an Add body). For Delete
// and Move-only operations content is empty.
//
// Paths are repo-relative when possible. The parser canonicalizes
// absolute paths against the session's cwd so attribution downstream
// keys consistently with how the scorer normalizes its own diff
// output.
type applyPatchFile struct {
	op        applyPatchOp
	path      string // primary path (repo-relative; existing path on Move)
	movedTo   string // destination path on Move; empty otherwise
	content   string // newline-joined Add/Update lines; empty for Delete
}

// parseApplyPatchEnvelope reads the *** Begin Patch / *** End Patch
// grammar Codex writes to tool_input.command and returns one record
// per file affected. The Codex envelope is line-oriented and minimally
// punctuated:
//
//	*** Begin Patch
//	*** Add File: <path>
//	+line one
//	+line two
//	*** Update File: <path>
//	@@
//	 context line
//	-removed line
//	+inserted line
//	*** Delete File: <path>
//	*** End Patch
//
// Add/Update bodies share the '+' prefix for new lines and ' ' for
// retained context; '-' lines are removed and contribute nothing to
// our line-level evidence. The envelope can include multiple file
// sections in a single command.
//
// Unknown headers and malformed lines are skipped (a corrupt section
// does not poison the rest of the envelope). repoRoot is used to
// rewrite absolute paths to repo-relative form so per-file matching
// downstream sees the same shape as Claude's tool_input.file_path.
func parseApplyPatchEnvelope(envelope, repoRoot string) []applyPatchFile {
	lines := strings.Split(envelope, "\n")
	var (
		out     []applyPatchFile
		current *applyPatchFile
		body    []string
	)

	flush := func() {
		if current == nil {
			return
		}
		current.content = joinPatchBody(body, current.op)
		out = append(out, *current)
		current = nil
		body = nil
	}

	for _, raw := range lines {
		switch {
		case strings.HasPrefix(raw, "*** Begin Patch"),
			strings.HasPrefix(raw, "*** End Patch"):
			flush()
		case strings.HasPrefix(raw, "*** Add File: "):
			flush()
			current = &applyPatchFile{
				op:   applyPatchOpAdd,
				path: normalizePatchPath(strings.TrimPrefix(raw, "*** Add File: "), repoRoot),
			}
		case strings.HasPrefix(raw, "*** Update File: "):
			flush()
			current = &applyPatchFile{
				op:   applyPatchOpUpdate,
				path: normalizePatchPath(strings.TrimPrefix(raw, "*** Update File: "), repoRoot),
			}
		case strings.HasPrefix(raw, "*** Delete File: "):
			flush()
			out = append(out, applyPatchFile{
				op:   applyPatchOpDelete,
				path: normalizePatchPath(strings.TrimPrefix(raw, "*** Delete File: "), repoRoot),
			})
		case strings.HasPrefix(raw, "*** Move to: "):
			// "Move to:" amends the most recent header rather than
			// introducing a fresh section. Codex emits it under either
			// an Update or an Add header when the file is being
			// renamed.
			if current != nil {
				current.op = applyPatchOpMove
				current.movedTo = normalizePatchPath(strings.TrimPrefix(raw, "*** Move to: "), repoRoot)
			}
		case current != nil:
			body = append(body, raw)
		}
	}
	flush()
	return out
}

// joinPatchBody assembles the new-line content for an Add or Update
// section. Add bodies prefix every line with '+'; Update bodies
// interleave '+', '-', and ' ' lines plus '@@' hunk markers. We strip
// the prefix when present and join with '\n' so the resulting blob
// looks the way a Write tool's `content` field would.
func joinPatchBody(body []string, op applyPatchOp) string {
	// Move is treated as a body-bearing op because the rename can
	// arrive alongside content changes ("*** Update File: <src>" +
	// "*** Move to: <dst>" + hunk lines), and the destination half
	// needs that content when buildApplyPatchEvents splits the Move
	// into a delete + add pair.
	if len(body) == 0 || (op != applyPatchOpAdd && op != applyPatchOpUpdate && op != applyPatchOpMove) {
		return ""
	}
	out := make([]string, 0, len(body))
	for _, line := range body {
		switch {
		case strings.HasPrefix(line, "+"):
			out = append(out, strings.TrimPrefix(line, "+"))
		case strings.HasPrefix(line, "-"):
			// '-' lines are removed by the patch; they are not part
			// of the new content and do not contribute lines to the
			// attribution candidate set.
		case strings.HasPrefix(line, "@@"):
			// Hunk header. Carries no content.
		case strings.HasPrefix(line, " "):
			// Retained context. Skipped so the blob captures only
			// lines the AI is responsible for in this turn.
		default:
			// Lines that do not match any expected marker are kept
			// verbatim only for Add (or Move-with-Add-style) bodies,
			// where every body line is implicitly new content even
			// when '+' is missing (e.g. trailing blank lines that
			// Codex emits without a prefix).
			if op == applyPatchOpAdd || op == applyPatchOpMove {
				out = append(out, line)
			}
		}
	}
	return strings.Join(out, "\n")
}

// normalizePatchPath turns the path Codex prints in a section header
// into a repo-relative form when possible.
//
// Codex writes absolute paths from the desktop app's runtime (e.g.
// `/private/tmp/codex-hook-probe/repo/main.go`) and repo-relative
// paths from the standalone CLI (e.g. `main.go`). Downstream
// attribution keys files by repo-relative path, so we strip the
// repoRoot prefix when it matches. Paths that already look relative
// pass through unchanged. Paths outside the repo (or that fail to
// relativize) are returned as-is; the scorer will simply not match
// them.
func normalizePatchPath(raw, repoRoot string) string {
	p := strings.TrimSpace(raw)
	if p == "" {
		return p
	}
	if !filepath.IsAbs(p) {
		return filepath.ToSlash(filepath.Clean(p))
	}
	if repoRoot == "" {
		return filepath.ToSlash(filepath.Clean(p))
	}
	rel, err := filepath.Rel(repoRoot, p)
	if err != nil || isOutsideRepo(rel) {
		return filepath.ToSlash(filepath.Clean(p))
	}
	return filepath.ToSlash(rel)
}

// isOutsideRepo returns true if a path returned by filepath.Rel
// escapes the repo root via one or more ".." segments. Checking for a
// plain HasPrefix("..") catches legitimate repo-internal names that
// happen to start with two dots (for example "..generated/file.go"),
// so we instead match either the literal ".." or a ".." segment
// followed by the OS path separator.
func isOutsideRepo(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

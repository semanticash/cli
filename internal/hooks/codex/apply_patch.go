package codex

import (
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/platform"
)

// applyPatchOp identifies a file operation in a patch envelope.
type applyPatchOp int

const (
	applyPatchOpUnknown applyPatchOp = iota
	applyPatchOpAdd
	applyPatchOpUpdate
	applyPatchOpDelete
	applyPatchOpMove
)

// applyPatchFile contains one parsed file change. Paths are repo-relative
// when possible; content and removed contain only changed lines.
type applyPatchFile struct {
	op      applyPatchOp
	path    string // primary path (repo-relative; existing path on Move)
	movedTo string // destination path on Move; empty otherwise
	content string // newline-joined '+' lines; empty for Delete
	removed string // newline-joined '-' lines; empty for Add and Delete
}

// parseApplyPatchEnvelope returns one record per file in a Codex patch:
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
// Unknown sections are skipped. Absolute paths under repoRoot become relative.
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
		current.removed = joinRemovedBody(body, current.op)
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
			// Move amends the current file section.
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

// joinRemovedBody returns removed lines for update and move sections.
func joinRemovedBody(body []string, op applyPatchOp) string {
	if len(body) == 0 || (op != applyPatchOpUpdate && op != applyPatchOpMove) {
		return ""
	}
	out := make([]string, 0, len(body))
	for _, line := range body {
		if strings.HasPrefix(line, "-") {
			out = append(out, strings.TrimPrefix(line, "-"))
		}
	}
	return strings.Join(out, "\n")
}

// joinPatchBody returns added lines in the shared Write payload shape.
func joinPatchBody(body []string, op applyPatchOp) string {
	// A move may also contain edits for the destination.
	if len(body) == 0 || (op != applyPatchOpAdd && op != applyPatchOpUpdate && op != applyPatchOpMove) {
		return ""
	}
	out := make([]string, 0, len(body))
	for _, line := range body {
		switch {
		case strings.HasPrefix(line, "+"):
			out = append(out, strings.TrimPrefix(line, "+"))
		case strings.HasPrefix(line, "-"):
			// Removed lines are not part of the new content.
		case strings.HasPrefix(line, "@@"):
			// Hunk header.
		case strings.HasPrefix(line, " "):
			// Retained context.
		default:
			// Add and move bodies may contain unprefixed lines.
			if op == applyPatchOpAdd || op == applyPatchOpMove {
				out = append(out, line)
			}
		}
	}
	return strings.Join(out, "\n")
}

// normalizePatchPath makes paths under repoRoot relative and preserves paths
// outside the repository.
func normalizePatchPath(raw, repoRoot string) string {
	p := strings.TrimSpace(raw)
	if p == "" {
		return p
	}
	// Recognize native and MSYS absolute paths on every host.
	if !platform.LooksAbsolutePath(p) {
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

// isOutsideRepo reports whether filepath.Rel escaped the repository root.
func isOutsideRepo(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

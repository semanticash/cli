package intentgap

import (
	"regexp"
	"strings"
)

// FileCategory classifies a changed file so candidate generation can
// reason about category coverage ("intent asked for a test; no test
// file changed"). Categories are derived deterministically from the
// path, never from file content.
type FileCategory string

const (
	CatCode   FileCategory = "code"
	CatTest   FileCategory = "test"
	CatDoc    FileCategory = "doc"
	CatConfig FileCategory = "config"
	CatSchema FileCategory = "schema"
)

// HunkDirection labels a hunk by shape, not by semantic intent. A
// unified-diff hunk header carries pre/post line counts that include
// context, so the counts alone tell you whether the pre or post side
// is empty — not whether the agent was rewriting, adding-with-context,
// or removing-with-context. The three values are inferred from those
// counts:
//
//   - HunkAdded:   pre-image side is empty (postCount > 0, preCount == 0).
//   - HunkRemoved: post-image side is empty (postCount == 0, preCount > 0).
//   - HunkChanged: both sides are non-empty. The hunk touches existing
//     lines with context; whether the body adds, removes, or rewrites
//     is a per-line property the hunk body carries, not something the
//     direction enum claims.
type HunkDirection string

const (
	HunkAdded   HunkDirection = "added"
	HunkRemoved HunkDirection = "removed"
	HunkChanged HunkDirection = "changed"
)

// ChangedHunk is one diff hunk inside one ChangedFile. StartLine and
// EndLine are inclusive 1-indexed positions: the new-side (post-image)
// positions for surviving hunks, and the old-side (pre-image) anchor
// for pure-removal hunks and deleted files where the new side does
// not exist. Body holds the raw hunk text including leading space /
// + / - markers so retrieval and the verifier can search and display
// it without losing the change shape.
type ChangedHunk struct {
	StartLine int
	EndLine   int
	Direction HunkDirection
	Body      string
}

// ChangedFile groups every hunk that touched one path, together with
// the deterministic FileCategory. Path uses forward slashes; for a
// surviving file it is the new-side path from `+++ b/<path>`, for a
// deleted file (Deleted=true) it is the old-side path from
// `--- a/<path>` because there is no new side. Retrieval needs both
// kinds so it can satisfy an intent like "delete X" without spuriously
// emitting a Track A "nothing addressed this ask" diagnostic; the
// adjudicator separately enforces that final under_impl findings cite
// regions that still exist in the new-side diff.
type ChangedFile struct {
	Path     string
	Category FileCategory
	Deleted  bool
	Hunks    []ChangedHunk
}

// ChangeLedger is the diff-side view consumed by retrieval, candidate
// generation, and verifier packet assembly. ByPath is a lookup index
// over Files; both are populated by BuildChangeLedger.
type ChangeLedger struct {
	Files  []ChangedFile
	ByPath map[string]*ChangedFile
}

// hunkHeaderFull captures both the pre-image and post-image line
// counts so we can classify hunk direction. Group 1 / 2 are the pre
// start and count; group 3 / 4 are the post start and count. A
// missing count defaults to 1 per git's convention.
var hunkHeaderFull = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// diffOldFileHeader matches the per-file `--- a/<path>` line so we
// can recover the pre-image path. The regex is only consulted on the
// deletion path (the new-side header was `/dev/null`); surviving
// files use the new-side path from `+++ b/<path>` for forward
// compatibility with downstream consumers that key on the post-image.
var diffOldFileHeader = regexp.MustCompile(`^--- (?:a/)?(.+)$`)

// BuildChangeLedger parses a unified diff into a ChangeLedger. It is
// the only ledger entry point callers should use. The function is
// deterministic and stays the source of truth for category rules;
// retrieval and candidate generators read the ledger they get back,
// they do not re-derive categories.
//
// ByPath is built after all Files appends complete so the pointers it
// stores remain valid: append can reallocate the underlying array,
// and an index map captured mid-loop would dangle.
func BuildChangeLedger(diff []byte) ChangeLedger {
	ledger := ChangeLedger{ByPath: map[string]*ChangedFile{}}
	if len(diff) == 0 {
		return ledger
	}

	var (
		currentFile *ChangedFile
		currentHunk *ChangedHunk
		hunkBody    strings.Builder
		oldPath     string // captured from `--- a/<path>` for deletion fallback

		// Running line counts for the open hunk. A unified-diff hunk
		// owns exactly preTotal pre-image lines and postTotal post-image
		// lines; until both totals are satisfied, every subsequent line
		// — including ones whose payload looks like `+++ foo` or
		// `--- foo` — is body, not the next file's header. Recognizing
		// file headers mid-hunk would prematurely finalize and corrupt
		// the ledger; well-formed `+` / `-` / ` ` body lines drive
		// these counters until the hunk is satisfied.
		preConsumed, postConsumed int
		preTotal, postTotal       int
	)

	finalizeHunk := func() {
		if currentFile == nil || currentHunk == nil {
			return
		}
		currentHunk.Body = hunkBody.String()
		currentFile.Hunks = append(currentFile.Hunks, *currentHunk)
		currentHunk = nil
		hunkBody.Reset()
		preConsumed, postConsumed = 0, 0
		preTotal, postTotal = 0, 0
	}

	hunkSatisfied := func() bool {
		return currentHunk == nil ||
			(preConsumed >= preTotal && postConsumed >= postTotal)
	}

	finalizeFile := func() {
		finalizeHunk()
		if currentFile == nil {
			return
		}
		// Files with only a header (no hunks) shouldn't be retained;
		// they happen on pure rename/mode-change diffs that don't
		// produce content changes anywhere in the ledger.
		if len(currentFile.Hunks) > 0 {
			ledger.Files = append(ledger.Files, *currentFile)
		}
		currentFile = nil
	}

	for _, line := range strings.Split(string(diff), "\n") {
		// File and hunk headers may only be recognized when no open
		// hunk is still consuming its declared pre/post line count.
		// Inside an open hunk, a payload line like `+++ foo` (the
		// added text of a line whose content begins with `++ `) or
		// `--- foo` is body, never a file header.
		if hunkSatisfied() {
			if m := diffOldFileHeader.FindStringSubmatch(line); m != nil && !strings.HasPrefix(line, "+++ ") {
				// `--- a/<path>` arrives before its `+++` partner;
				// remember the old path so a subsequent
				// `+++ /dev/null` can use it.
				oldPath = strings.TrimSpace(m[1])
				if oldPath == "/dev/null" {
					oldPath = ""
				}
				continue
			}
			if m := diffFileHeader.FindStringSubmatch(line); m != nil {
				finalizeFile()
				path := strings.TrimSpace(m[1])
				deleted := false
				if path == "/dev/null" {
					if oldPath == "" {
						currentFile = nil
						oldPath = ""
						continue
					}
					path = oldPath
					deleted = true
				}
				currentFile = &ChangedFile{
					Path:     path,
					Category: categorize(path),
					Deleted:  deleted,
				}
				oldPath = ""
				continue
			}
			if currentFile == nil {
				continue
			}
			if m := hunkHeaderFull.FindStringSubmatch(line); m != nil {
				finalizeHunk()
				preCount := 1
				if m[2] != "" {
					preCount = atoi(m[2])
				}
				postStart := atoi(m[3])
				postCount := 1
				if m[4] != "" {
					postCount = atoi(m[4])
				}
				// Direction is computed from the raw pre/post counts.
				// Anchoring the line range for a pure-removal hunk
				// (postCount == 0) is a separate concern, done after so
				// the direction stays accurate.
				direction := hunkDirection(preCount, postCount)
				anchorStart, anchorEnd := postStart, postStart+postCount-1
				if postStart == 0 || postCount == 0 {
					anchorStart = atoi(m[1])
					anchorEnd = anchorStart
				}
				currentHunk = &ChangedHunk{
					StartLine: anchorStart,
					EndLine:   anchorEnd,
					Direction: direction,
				}
				preTotal, postTotal = preCount, postCount
				preConsumed, postConsumed = 0, 0
				hunkBody.WriteString(line)
				hunkBody.WriteByte('\n')
				continue
			}
		}
		if currentHunk != nil {
			// Inside an open hunk: append to body and advance the
			// pre/post counters by the line marker so we know when
			// the hunk is satisfied. Lines we cannot classify (empty
			// strings from split, malformed body) advance neither
			// counter; well-formed git diffs avoid this case.
			hunkBody.WriteString(line)
			hunkBody.WriteByte('\n')
			if len(line) > 0 {
				switch line[0] {
				case ' ':
					preConsumed++
					postConsumed++
				case '+':
					postConsumed++
				case '-':
					preConsumed++
				case '\\':
					// `\ No newline at end of file` marker: belongs to
					// the hunk body but does not advance either side.
				}
			}
		}
	}
	finalizeFile()

	// Build ByPath only after Files is fully populated. Indexing into
	// the slice during the loop is unsafe: append can reallocate the
	// backing array, leaving previously-stored *ChangedFile pointers
	// pointing at the old array.
	for i := range ledger.Files {
		ledger.ByPath[ledger.Files[i].Path] = &ledger.Files[i]
	}
	return ledger
}

// hunkDirection derives a shape label from the hunk's pre/post line
// counts. See HunkDirection's doc comment for what each value claims
// (and what it does not).
func hunkDirection(preCount, postCount int) HunkDirection {
	switch {
	case preCount == 0 && postCount > 0:
		return HunkAdded
	case preCount > 0 && postCount == 0:
		return HunkRemoved
	default:
		return HunkChanged
	}
}

// categorize maps a file path to a FileCategory using the
// path-based rules documented in the candidate-first plan. The rules
// are intentionally narrow: anything we cannot confidently put in a
// specialized bucket stays "code", and the candidate generators will
// not falsely conclude a test/doc/config/schema category is missing
// when the project simply does not use that category in this PR.
func categorize(path string) FileCategory {
	lower := strings.ToLower(path)
	base := lower
	if idx := strings.LastIndexByte(lower, '/'); idx >= 0 {
		base = lower[idx+1:]
	}

	if isTestPath(lower, base) {
		return CatTest
	}
	// Schema must beat doc because well-named schema files often live
	// under docs/ (api/docs/schemas/intent_gap.schema.json is the
	// canonical example) and would otherwise be miscategorized as doc.
	if strings.HasSuffix(base, ".sql") ||
		strings.HasSuffix(base, ".schema.json") ||
		strings.Contains(base, "schema") {
		return CatSchema
	}
	if strings.HasSuffix(base, ".md") || hasPathSegment(lower, "docs") {
		return CatDoc
	}
	if strings.HasSuffix(base, ".json") ||
		strings.HasSuffix(base, ".yaml") ||
		strings.HasSuffix(base, ".yml") ||
		strings.HasSuffix(base, ".toml") ||
		strings.HasPrefix(base, ".env") {
		return CatConfig
	}
	return CatCode
}

// isTestPath recognizes a handful of widely used test conventions
// across Go, Python, and JS/TS. The list is deliberately conservative;
// false-positive test classification weakens Track B's
// "test missing for this intent" predicate.
func isTestPath(lowerPath, lowerBase string) bool {
	if strings.HasSuffix(lowerBase, "_test.go") {
		return true
	}
	if strings.HasSuffix(lowerBase, ".test.go") {
		return true
	}
	if hasPathSegment(lowerPath, "tests") || hasPathSegment(lowerPath, "test") {
		return true
	}
	for _, ext := range []string{
		".test.ts", ".test.tsx", ".test.js", ".test.jsx",
		".spec.ts", ".spec.tsx", ".spec.js", ".spec.jsx",
	} {
		if strings.HasSuffix(lowerBase, ext) {
			return true
		}
	}
	return false
}

// hasPathSegment reports whether any forward-slash segment of path
// equals segment. Used so "tests/foo" matches but "testify" does not.
func hasPathSegment(path, segment string) bool {
	for _, p := range strings.Split(path, "/") {
		if p == segment {
			return true
		}
	}
	return false
}

package intentgap

import (
	"sort"
	"strings"
)

// HunkRef is a citation-shaped reference into a ChangedHunk. The
// verifier packet attaches a small list of these so the model sees
// exactly which scopes the retrieval step thought were relevant.
type HunkRef struct {
	File      string
	StartLine int
	EndLine   int
}

// RetrievedScope is the per-intent retrieval output consumed by the
// candidate generators. Files lists paths whose composite score met
// or exceeded RetrievalScoreThreshold (ordered desc by score).
// NearMisses lists paths with a positive but below-threshold score
// so Track A candidates can show the model the closest things the
// diff changed even when nothing met the threshold. TestPaths /
// DocPaths are projections of Files filtered by category for the
// "category missing" Track B predicate.
//
// Scores keeps every file → score pair the retrieval considered
// (positive scores only). It is intended for diagnostic logging,
// not for downstream logic.
type RetrievedScope struct {
	IntentID   string
	Files      []string
	HunkRefs   []HunkRef
	ActionIDs  []string
	TestPaths  []string
	DocPaths   []string
	NearMisses []string
	Scores     map[string]float64
}

// Retrieval tuning constants. See the candidate-first plan §Step 4
// Retrieval for the rationale. The thresholds are kept conservative
// so the verifier remains the safety net: a near-miss path going
// into Files is more recoverable than a real match landing in
// NearMisses and never reaching a candidate.
const (
	RetrievalScoreThreshold    = 0.3
	MaxRetrievedFilesPerIntent = 8
	MaxNearMissesPerIntent     = 5
	MaxHunkRefsPerScope        = 6
	MaxActionIDsPerScope       = 8

	// ActionAdjacencyWindowSec is the symmetric ±window around an
	// intent's TurnTS within which an action's TS qualifies as
	// "adjacent." 30 minutes covers typical conversational gaps
	// between a user asking for something and the agent acting on
	// it without sweeping in unrelated work from a later session.
	ActionAdjacencyWindowSec = 30 * 60

	// Signal weights. Final per-file score is the weighted sum of
	// the four signals, each normalized to [0,1].
	SignalWeightLexicalPath     = 1.0
	SignalWeightLexicalHunk     = 0.8
	SignalWeightActionAdjacency = 0.6
	SignalWeightTestDocSibling  = 0.3

	// minIntentTokenLen filters out short words ("the", "and", "is")
	// that produce too many spurious matches. 4 is permissive enough
	// to keep "test", "json", "code"; restrictive enough to drop
	// "and", "for", "but".
	minIntentTokenLen = 4
)

// BuildRetrieval computes a RetrievedScope for one intent against
// the diff and action ledger. The function is deterministic and
// makes no network or LLM calls; signal computation is plain string
// matching plus a numeric time-window check.
//
// The intent's TurnTS drives action adjacency; the intent's Summary,
// Excerpt, and Hint.HintFiles are tokenized together and used as the
// lexical query against changed-file paths and hunk bodies.
func BuildRetrieval(intent IntentItem, change ChangeLedger, action ActionLedger) RetrievedScope {
	scope := RetrievedScope{
		IntentID: intent.ID,
		Scores:   map[string]float64{},
	}
	if len(change.Files) == 0 {
		return scope
	}

	tokens := tokenizeIntent(intent.Summary + " " + intent.Excerpt + " " + strings.Join(intent.Hint.HintFiles, " ") + " " + strings.Join(intent.Hint.HintKeywords, " "))

	pathScore := make(map[string]float64, len(change.Files))
	hunkScore := make(map[string]float64, len(change.Files))
	for _, f := range change.Files {
		pathScore[f.Path] = lexicalPathScore(f.Path, tokens, intent.Hint.HintFiles)
		hunkScore[f.Path] = lexicalHunkScore(f.Hunks, tokens)
	}

	// Action adjacency: a file gets the binary signal when it has
	// any captured action whose timestamp falls inside the
	// adjacency window around the intent's turn.
	adj := make(map[string]bool, len(change.Files))
	windowStart := intent.TurnTS - int64(ActionAdjacencyWindowSec)
	windowEnd := intent.TurnTS + int64(ActionAdjacencyWindowSec)
	for path, ids := range action.ByFile {
		for _, id := range ids {
			a := action.ByID[id]
			if a.TS >= windowStart && a.TS <= windowEnd {
				adj[path] = true
				break
			}
		}
	}

	// Sibling signal: test/doc files in the same package directory
	// as a positively-scored CODE file get a small bonus. The
	// purpose is to keep tests and docs relevant to a refactor in
	// scope even when their own paths share no tokens with the
	// intent text.
	codeDirsWithScore := map[string]bool{}
	for _, f := range change.Files {
		if f.Category == CatCode && (pathScore[f.Path] > 0 || hunkScore[f.Path] > 0) {
			codeDirsWithScore[parentDir(f.Path)] = true
		}
	}

	for _, f := range change.Files {
		var total float64
		total += pathScore[f.Path] * SignalWeightLexicalPath
		total += hunkScore[f.Path] * SignalWeightLexicalHunk
		if adj[f.Path] {
			total += SignalWeightActionAdjacency
		}
		if (f.Category == CatTest || f.Category == CatDoc) && codeDirsWithScore[parentDir(f.Path)] {
			total += SignalWeightTestDocSibling
		}
		if total > 0 {
			scope.Scores[f.Path] = total
		}
	}

	// Partition by threshold; sort by descending score; apply caps.
	var above, below []fileScore
	for path, s := range scope.Scores {
		if s >= RetrievalScoreThreshold {
			above = append(above, fileScore{path, s})
		} else {
			below = append(below, fileScore{path, s})
		}
	}
	sortByScoreDesc(above)
	sortByScoreDesc(below)
	if len(above) > MaxRetrievedFilesPerIntent {
		above = above[:MaxRetrievedFilesPerIntent]
	}
	if len(below) > MaxNearMissesPerIntent {
		below = below[:MaxNearMissesPerIntent]
	}
	scope.Files = make([]string, 0, len(above))
	for _, fs := range above {
		scope.Files = append(scope.Files, fs.path)
	}
	scope.NearMisses = make([]string, 0, len(below))
	for _, fs := range below {
		scope.NearMisses = append(scope.NearMisses, fs.path)
	}

	// TestPaths / DocPaths are subsets of Files filtered by
	// category, preserving Files ordering.
	for _, p := range scope.Files {
		if f := change.ByPath[p]; f != nil {
			switch f.Category {
			case CatTest:
				scope.TestPaths = append(scope.TestPaths, p)
			case CatDoc:
				scope.DocPaths = append(scope.DocPaths, p)
			}
		}
	}

	// HunkRefs are the top-scoring hunks across kept Files. Per-hunk
	// score is the number of intent tokens that appear in the hunk
	// body — a hunk that mentions more of the intent's vocabulary
	// is a better citation target.
	type hunkCandidate struct {
		ref   HunkRef
		score int
	}
	var hunks []hunkCandidate
	for _, p := range scope.Files {
		f := change.ByPath[p]
		if f == nil {
			continue
		}
		for _, h := range f.Hunks {
			n := countTokensInText(strings.ToLower(h.Body), tokens)
			if n > 0 {
				hunks = append(hunks, hunkCandidate{
					ref:   HunkRef{File: p, StartLine: h.StartLine, EndLine: h.EndLine},
					score: n,
				})
			}
		}
	}
	sort.SliceStable(hunks, func(i, j int) bool { return hunks[i].score > hunks[j].score })
	if len(hunks) > MaxHunkRefsPerScope {
		hunks = hunks[:MaxHunkRefsPerScope]
	}
	for _, h := range hunks {
		scope.HunkRefs = append(scope.HunkRefs, h.ref)
	}

	// Time-adjacent actions on Files. The cap is per scope, not per
	// file, so a single noisy file cannot starve out the rest.
	for _, p := range scope.Files {
		if len(scope.ActionIDs) >= MaxActionIDsPerScope {
			break
		}
		for _, id := range action.ByFile[p] {
			if len(scope.ActionIDs) >= MaxActionIDsPerScope {
				break
			}
			a := action.ByID[id]
			if a.TS >= windowStart && a.TS <= windowEnd {
				scope.ActionIDs = append(scope.ActionIDs, id)
			}
		}
	}

	return scope
}

// tokenizeIntent splits the intent text into lowercase alphanumeric
// tokens of at least minIntentTokenLen characters. Duplicates are
// removed so a repeated word does not artificially inflate scores
// downstream.
func tokenizeIntent(s string) []string {
	var out []string
	seen := map[string]bool{}
	var b strings.Builder
	flush := func() {
		t := b.String()
		b.Reset()
		if len(t) < minIntentTokenLen {
			return
		}
		if seen[t] {
			return
		}
		seen[t] = true
		out = append(out, t)
	}
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// lexicalPathScore is the fraction of intent tokens that appear as
// substrings of the lowercased file path, with an additional boost
// for HintFiles that match the path exactly or as a substring. The
// final value is clamped to [0,1] so this signal cannot dominate
// the composite score.
func lexicalPathScore(path string, tokens []string, hintFiles []string) float64 {
	lower := strings.ToLower(path)
	var matches int
	for _, t := range tokens {
		if strings.Contains(lower, t) {
			matches++
		}
	}
	var ratio float64
	if len(tokens) > 0 {
		ratio = float64(matches) / float64(len(tokens))
	}
	for _, hf := range hintFiles {
		lowerHF := strings.ToLower(strings.TrimSpace(hf))
		if lowerHF == "" {
			continue
		}
		if lowerHF == lower {
			ratio += 1.0
			break
		}
		if strings.Contains(lower, lowerHF) {
			ratio += 0.5
		}
	}
	if ratio > 1.0 {
		ratio = 1.0
	}
	return ratio
}

// lexicalHunkScore is the fraction of intent tokens that appear at
// least once across the file's combined hunk bodies. Multi-hunk
// files do not get a count boost from the same token repeating —
// distinct tokens matched is the signal, not repetition.
func lexicalHunkScore(hunks []ChangedHunk, tokens []string) float64 {
	if len(tokens) == 0 || len(hunks) == 0 {
		return 0
	}
	var body strings.Builder
	for _, h := range hunks {
		body.WriteString(h.Body)
		body.WriteByte('\n')
	}
	lower := strings.ToLower(body.String())
	matches := countTokensInText(lower, tokens)
	return float64(matches) / float64(len(tokens))
}

// countTokensInText counts how many distinct tokens appear at least
// once as substrings of text. The text is assumed to be lowercased
// already; passing a mixed-case text just dilutes recall.
func countTokensInText(lowerText string, tokens []string) int {
	n := 0
	for _, t := range tokens {
		if strings.Contains(lowerText, t) {
			n++
		}
	}
	return n
}

// parentDir returns the path's parent directory, normalized to
// forward slashes. Files at the repository root return the empty
// string so directory-based grouping does not invent a sentinel.
func parentDir(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return ""
}

// fileScore is the partition entry used while ranking retrieval
// results. Kept package-private; the public surface is the
// RetrievedScope produced by BuildRetrieval.
type fileScore struct {
	path  string
	score float64
}

// sortByScoreDesc sorts a fileScore slice from highest to lowest
// score, breaking ties by file path to keep output deterministic.
func sortByScoreDesc(in []fileScore) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].score == in[j].score {
			return in[i].path < in[j].path
		}
		return in[i].score > in[j].score
	})
}

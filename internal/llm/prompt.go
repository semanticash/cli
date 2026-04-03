package llm

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// SystemPrompt instructs the model to analyze a development session.
const SystemPrompt = `Analyze this commit and generate a structured summary.

<transcript>
%s
</transcript>

<diff>
%s
</diff>

<stats>
%s
</stats>

Return a JSON object with this exact structure:
{
  "title": "Short title for this playbook (max 10 words, like a commit message)",
  "intent": "What the developer was trying to accomplish (1-2 sentences)",
  "outcome": "What was actually achieved (1-2 sentences)",
  "learnings": ["Codebase-specific patterns, conventions, or gotchas discovered"],
  "friction": ["Problems, blockers, or annoyances encountered"],
  "open_items": ["Tech debt, unfinished work, or things to revisit later"],
  "keywords": ["5-10 search terms a developer might use to find this work later"]
}

Guidelines:
- The title should be a concise, descriptive label - not a full sentence
- Be concise but specific
- If the transcript is empty, infer intent and outcome from the diff and stats alone
- Friction should capture both blockers and minor annoyances
- Open items are things intentionally deferred, not failures
- Keywords should include synonyms, related concepts, and technology names not already in the other fields
- Empty arrays are fine if a category doesn't apply
- Return ONLY the JSON object, no markdown formatting or explanation`

// NarrativeResult holds the structured LLM response.
type NarrativeResult struct {
	Title     string   `json:"title"`
	Intent    string   `json:"intent"`
	Outcome   string   `json:"outcome"`
	Learnings []string `json:"learnings"`
	Friction  []string `json:"friction"`
	OpenItems []string `json:"open_items"`
	Keywords  []string `json:"keywords"`
}

// TranscriptEntry is a lightweight event for the condensed transcript.
type TranscriptEntry struct {
	Role     string
	Summary  string
	ToolName string
	FilePath string
}

// ExplainContext is the subset of explain stats needed for the prompt.
type ExplainContext struct {
	FilesChanged int     `json:"files_changed"`
	LinesAdded   int     `json:"lines_added"`
	LinesDeleted int     `json:"lines_deleted"`
	AIPercentage float64 `json:"ai_percentage"`
	AILines      int     `json:"ai_lines"`
	HumanLines   int     `json:"human_lines"`
	SessionCount int     `json:"session_count"`
	RootSessions int     `json:"root_sessions"`
	Subagents    int     `json:"subagents"`
	TopFiles     []struct {
		Path       string  `json:"path"`
		Added      int     `json:"added"`
		Deleted    int     `json:"deleted"`
		TotalLines int     `json:"total_lines"`
		AILines    int     `json:"ai_lines"`
		HumanLines int     `json:"human_lines"`
		AIPercent  float64 `json:"ai_percentage"`
	} `json:"top_files"`
}

const (
	maxDiffLen                      = 12_000
	maxCommitMsgSummaryFiles        = 16
	maxCommitMsgExcerptFiles        = 8
	maxCommitMsgExcerptLinesPerFile = 16
)

// FormatCondensedTranscript formats transcript entries into a human-readable
// format for LLM consumption.
func FormatCondensedTranscript(entries []TranscriptEntry) string {
	var sb strings.Builder

	for i, e := range entries {
		if i > 0 {
			sb.WriteString("\n")
		}

		switch {
		case e.ToolName != "":
			sb.WriteString("[Tool] ")
			sb.WriteString(e.ToolName)
			if e.FilePath != "" {
				sb.WriteString(": ")
				sb.WriteString(e.FilePath)
			}
			sb.WriteString("\n")
		case e.Role == "user":
			sb.WriteString("[User] ")
			sb.WriteString(e.Summary)
			sb.WriteString("\n")
		case e.Role == "assistant":
			sb.WriteString("[Assistant] ")
			sb.WriteString(e.Summary)
			sb.WriteString("\n")
		default:
			if e.Summary != "" {
				_, _ = fmt.Fprintf(&sb, "[%s] %s\n", e.Role, e.Summary)
			}
		}
	}

	return sb.String()
}

// CommitMsgPrompt instructs the model to generate a commit message.
const CommitMsgPrompt = `Write a git commit message for the following changes.

%s<change_summary>
%s
</change_summary>

<diff_excerpt>
%s
</diff_excerpt>

Rules:
- Return exactly one line.
- Most commits should be one short sentence.
- If the change set clearly spans two distinct concerns, you may return two short
  adjacent sentences on the same line.
- Never return more than two sentences.
- Keep each sentence concise and plain text.
- Write like a developer - terse, no filler
- Use third-person singular present tense (e.g. "Adds", "Fixes", "Updates")
- Never use backticks, even around identifiers like t.Fatalf or _ =
- Do not use quotes or special shell characters in the message
- Do not mention AI, tools, or automation
- Transcript context is supplemental. Base the message on the current change summary and diff excerpt.
- Do not overfit to the first or largest file if the summary shows broader code changes.
- Do not wrap in markdown code blocks
- Return ONLY the commit message text, nothing else`

// BuildCommitMsgPrompt assembles transcript context plus a bounded change
// summary and diff excerpt for commit message generation.
func BuildCommitMsgPrompt(transcript []TranscriptEntry, diff string) string {
	var contextBlock string
	if len(transcript) > 0 {
		contextBlock = fmt.Sprintf("<context>\n%s</context>\n\n", FormatCondensedTranscript(transcript))
	}

	summary, excerpt := buildCommitMsgDiffContext(diff)
	return fmt.Sprintf(CommitMsgPrompt, contextBlock, summary, excerpt)
}

// BuildUserPrompt assembles transcript, diff, and stats into the prompt.
func BuildUserPrompt(commitHash, subject string, stats ExplainContext, transcript []TranscriptEntry, diff string) string {
	transcriptText := FormatCondensedTranscript(transcript)

	if len(diff) > maxDiffLen {
		diff = diff[:maxDiffLen] + "\n... (truncated)"
	}

	statsJSON, _ := json.MarshalIndent(stats, "", "  ")

	return fmt.Sprintf(SystemPrompt, transcriptText, diff, string(statsJSON))
}

type commitMsgDiffFile struct {
	Path    string
	Status  string
	Added   int
	Deleted int
	Snippet []string
	IsDoc   bool
}

// buildCommitMsgDiffContext converts a raw unified diff into:
// - a compact summary of overall scope
// - a per-file excerpt that favors code over large doc-only changes
func buildCommitMsgDiffContext(diff string) (string, string) {
	files := parseCommitMsgDiff(diff)
	if len(files) == 0 {
		if len(diff) > maxDiffLen {
			diff = diff[:maxDiffLen] + "\n... (truncated)"
		}
		return "Files changed: unknown", diff
	}

	prioritized := prioritizeCommitMsgFiles(files)

	var totalAdded, totalDeleted, docCount int
	for _, f := range files {
		totalAdded += f.Added
		totalDeleted += f.Deleted
		if f.IsDoc {
			docCount++
		}
	}

	var summary strings.Builder
	fmt.Fprintf(&summary, "Files changed: %d\n", len(files))
	fmt.Fprintf(&summary, "Lines added: %d\n", totalAdded)
	fmt.Fprintf(&summary, "Lines deleted: %d\n", totalDeleted)
	if docCount > 0 && docCount < len(files) {
		fmt.Fprintf(&summary, "Non-doc files: %d\n", len(files)-docCount)
		fmt.Fprintf(&summary, "Doc files: %d\n", docCount)
	}
	summary.WriteString("Representative files:\n")
	for i, f := range prioritized {
		if i >= maxCommitMsgSummaryFiles {
			break
		}
		fmt.Fprintf(&summary, "- %s %s (+%d -%d)\n", f.Status, f.Path, f.Added, f.Deleted)
	}
	if len(files) > maxCommitMsgSummaryFiles {
		fmt.Fprintf(&summary, "- ... and %d more files\n", len(files)-maxCommitMsgSummaryFiles)
	}

	hasNonDocExcerpt := false
	for _, f := range prioritized {
		if len(f.Snippet) > 0 && !f.IsDoc {
			hasNonDocExcerpt = true
			break
		}
	}

	var excerpt strings.Builder
	shown := 0
	for _, f := range prioritized {
		if shown >= maxCommitMsgExcerptFiles {
			break
		}
		if len(f.Snippet) == 0 {
			continue
		}
		if hasNonDocExcerpt && f.IsDoc {
			continue
		}
		fmt.Fprintf(&excerpt, "File: %s (%s, +%d -%d)\n", f.Path, f.Status, f.Added, f.Deleted)
		for _, line := range f.Snippet {
			excerpt.WriteString(line)
			excerpt.WriteString("\n")
		}
		excerpt.WriteString("\n")
		shown++
	}
	if shown == 0 {
		if len(diff) > maxDiffLen {
			diff = diff[:maxDiffLen] + "\n... (truncated)"
		}
		return summary.String(), diff
	}

	return summary.String(), strings.TrimSpace(excerpt.String())
}

func parseCommitMsgDiff(diff string) []commitMsgDiffFile {
	lines := strings.Split(diff, "\n")
	var files []commitMsgDiffFile
	var cur *commitMsgDiffFile

	flush := func() {
		if cur == nil {
			return
		}
		cur.IsDoc = isDocPath(cur.Path)
		files = append(files, *cur)
		cur = nil
	}

	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			parts := strings.Fields(line)
			cur = &commitMsgDiffFile{Status: "modified"}
			if len(parts) >= 4 {
				if path := trimDiffPath(parts[3]); path != "" {
					cur.Path = path
				} else {
					cur.Path = trimDiffPath(parts[2])
				}
			}
			continue
		}
		if cur == nil {
			continue
		}

		switch {
		case strings.HasPrefix(line, "new file mode "):
			cur.Status = "added"
		case strings.HasPrefix(line, "deleted file mode "):
			cur.Status = "deleted"
		case strings.HasPrefix(line, "rename to "):
			cur.Status = "renamed"
			cur.Path = strings.TrimSpace(strings.TrimPrefix(line, "rename to "))
		case strings.HasPrefix(line, "+++ "):
			if path := trimDiffPath(strings.TrimSpace(strings.TrimPrefix(line, "+++ "))); path != "" {
				cur.Path = path
			}
		case strings.HasPrefix(line, "@@ "):
			cur.addSnippet(line)
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++ "):
			cur.Added++
			cur.addSnippet(line)
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "--- "):
			cur.Deleted++
			cur.addSnippet(line)
		}
	}
	flush()
	return files
}

func (f *commitMsgDiffFile) addSnippet(line string) {
	if len(f.Snippet) >= maxCommitMsgExcerptLinesPerFile {
		return
	}
	f.Snippet = append(f.Snippet, line)
}

// prioritizeCommitMsgFiles sorts files by likely commit-message relevance.
// When code and docs are mixed, code files come first; ties break by churn.
func prioritizeCommitMsgFiles(files []commitMsgDiffFile) []commitMsgDiffFile {
	prioritized := append([]commitMsgDiffFile(nil), files...)
	hasCode := false
	for _, f := range prioritized {
		if !f.IsDoc {
			hasCode = true
			break
		}
	}

	sort.SliceStable(prioritized, func(i, j int) bool {
		if hasCode && prioritized[i].IsDoc != prioritized[j].IsDoc {
			return !prioritized[i].IsDoc
		}
		churnI := prioritized[i].Added + prioritized[i].Deleted
		churnJ := prioritized[j].Added + prioritized[j].Deleted
		if churnI != churnJ {
			return churnI > churnJ
		}
		return prioritized[i].Path < prioritized[j].Path
	})

	return prioritized
}

func trimDiffPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || path == "/dev/null" {
		return ""
	}
	if strings.HasPrefix(path, "a/") || strings.HasPrefix(path, "b/") {
		return path[2:]
	}
	return path
}

func isDocPath(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".md", ".rst", ".adoc", ".txt":
		return true
	}
	base := strings.ToLower(filepath.Base(path))
	switch base {
	case "readme", "readme.md", "changelog", "changelog.md", "license", "license.md":
		return true
	}
	return strings.HasPrefix(path, "docs/")
}

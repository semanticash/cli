package llm

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// maxPRDiffLen is the soft character budget for the summary+excerpt passed to
// the PR prompt. Larger than the commit-msg limit because PRs span more files.
const maxPRDiffSummaryFiles = 24
const maxPRDiffExcerptFiles = 12

// SuggestPROutput is the structured JSON the LLM must return.
type SuggestPROutput struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// SuggestPRPrompt instructs the model to author a pull request.
const SuggestPRPrompt = `You are a PR author. Given a diff, commit subjects, and an optional PR template, write a pull request title and body.

<change_summary>
%s
</change_summary>

<diff_excerpt>
%s
</diff_excerpt>

<commit_subjects>
%s
</commit_subjects>

<pr_template>
%s
</pr_template>

Rules:
- Return a JSON object with exactly two keys: "title" and "body"
- Title: max 72 characters, imperative mood, no trailing period
- Body: if a PR template is provided, fill in its sections faithfully - do not add or remove sections
- Body: if no template, use: ## Summary (bullet points of logical changes), ## Test plan (testing notes)
- Summarize by logical change, not commit-by-commit
- Be specific about what changed and why, not vague
- Do not mention AI, tools, or automation
- Do not wrap the JSON in markdown code blocks
- Return ONLY the JSON object`

// BuildSuggestPRPrompt assembles the prompt for PR generation.
// It reuses the summary+excerpt strategy from commit-msg prompts.
func BuildSuggestPRPrompt(diff, commitSubjects, prTemplate string) string {
	summary, excerpt := buildPRDiffContext(diff)

	if prTemplate == "" {
		prTemplate = "(no template provided - use default structure: ## Summary, ## Test plan)"
	}

	return fmt.Sprintf(SuggestPRPrompt, summary, excerpt, commitSubjects, prTemplate)
}

// buildPRDiffContext reuses the commit-msg diff parsing with higher limits.
func buildPRDiffContext(diff string) (string, string) {
	files := parseCommitMsgDiff(diff)
	if len(files) == 0 {
		if len(diff) > 24_000 {
			diff = diff[:24_000] + "\n... (truncated)"
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
		if i >= maxPRDiffSummaryFiles {
			break
		}
		fmt.Fprintf(&summary, "- %s %s (+%d -%d)\n", f.Status, f.Path, f.Added, f.Deleted)
	}
	if len(files) > maxPRDiffSummaryFiles {
		fmt.Fprintf(&summary, "- ... and %d more files\n", len(files)-maxPRDiffSummaryFiles)
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
		if shown >= maxPRDiffExcerptFiles {
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
		if len(diff) > 24_000 {
			diff = diff[:24_000] + "\n... (truncated)"
		}
		return summary.String(), diff
	}

	return summary.String(), strings.TrimSpace(excerpt.String())
}

// ParseSuggestPROutput extracts the structured title/body from the LLM response.
func ParseSuggestPROutput(raw string) (*SuggestPROutput, error) {
	cleaned := extractJSONFromMarkdown(raw)
	var out SuggestPROutput
	if err := json.Unmarshal([]byte(cleaned), &out); err != nil {
		return nil, fmt.Errorf("parse PR JSON: %w", err)
	}
	// Sanitize title: take only the first line, trim whitespace.
	out.Title = strings.TrimSpace(strings.SplitN(out.Title, "\n", 2)[0])
	if out.Title == "" {
		return nil, fmt.Errorf("LLM returned empty title")
	}
	out.Body = strings.TrimSpace(out.Body)
	return &out, nil
}

// StripHTMLComments removes HTML comments while leaving the surrounding
// template content intact.
var htmlCommentRe = regexp.MustCompile(`<!--[\s\S]*?-->`)

func StripHTMLComments(s string) string {
	return strings.TrimSpace(htmlCommentRe.ReplaceAllString(s, ""))
}

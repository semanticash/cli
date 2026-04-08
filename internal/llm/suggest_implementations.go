package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SuggestImplementationOutput is the structured JSON the LLM must return
// for a single implementation.
type SuggestImplementationOutput struct {
	Title          string       `json:"title"`
	Summary        string       `json:"summary"`
	ReviewPriority []ReviewItem `json:"review_priority,omitempty"`
}

// ReviewItem ranks a file for review attention.
type ReviewItem struct {
	Priority string `json:"priority"` // "high", "medium", "low"
	Repo     string `json:"repo"`
	File     string `json:"file"`
	Reason   string `json:"reason"`
}

// SuggestMergeCandidateOutput is the structured JSON for merge suggestions
// across multiple implementations.
type SuggestMergeCandidateOutput struct {
	Titles []TitleSuggestion  `json:"titles,omitempty"`
	Merges []MergeSuggestion  `json:"merges,omitempty"`
}

// TitleSuggestion proposes a title for an untitled implementation.
type TitleSuggestion struct {
	ImplementationID string `json:"implementation_id"`
	Title            string `json:"title"`
}

// MergeSuggestion proposes merging two implementations.
type MergeSuggestion struct {
	ImplementationA string `json:"implementation_a"`
	ImplementationB string `json:"implementation_b"`
	Reason          string `json:"reason"`
}

// suggestImplementationPrompt instructs the model to analyze a single implementation.
const suggestImplementationPrompt = `You are an engineering analyst. Given a cross-repo implementation's timeline, repos, sessions, and commits, generate a concise title, summary, and review priority ranking.

<implementation>
State: %s
Repos: %s
Sessions: %d
Tokens: %s in / %s out

Commits:
%s

Timeline (recent events):
%s
</implementation>

Rules:
- Return a JSON object with exactly three keys: "title", "summary", "review_priority"
- Title: max 60 characters, describes the single logical change (e.g. "Migrate auth to OAuth2")
- Summary: 2-3 sentences describing what was done and why, across all repos
- review_priority: array of objects with "priority" (high/medium/low), "repo", "file", "reason"
- Focus on what a reviewer should look at first
- Be specific about cross-repo relationships
- Do not mention AI, tools, or automation
- Do not wrap the JSON in markdown code blocks
- Return ONLY the JSON object`

// suggestMergeCandidatesPrompt instructs the model to find merge/title candidates.
const suggestMergeCandidatesPrompt = `You are an engineering analyst. Given a list of implementations with their repos and recent activity, suggest:
1. Titles for untitled implementations
2. Pairs of implementations that should be merged (same logical effort split across two implementations)

<implementations>
%s
</implementations>

Rules:
- Return a JSON object with exactly two keys: "titles" and "merges"
- titles: array of {"implementation_id": "...", "title": "..."}. Only for implementations marked (untitled).
- merges: array of {"implementation_a": "...", "implementation_b": "...", "reason": "..."}. Only suggest merges when two implementations clearly represent the same logical effort.
- Be conservative with merge suggestions - only suggest when evidence is strong
- Titles: max 60 characters, imperative mood
- Do not wrap the JSON in markdown code blocks
- Return ONLY the JSON object`

// BuildSuggestImplementationPrompt assembles the prompt for a single implementation.
func BuildSuggestImplementationPrompt(
	state string,
	repos string,
	sessionCount int,
	tokensIn, tokensOut string,
	commits string,
	timeline string,
) string {
	return fmt.Sprintf(suggestImplementationPrompt,
		state, repos, sessionCount, tokensIn, tokensOut, commits, timeline)
}

// BuildSuggestMergeCandidatesPrompt assembles the prompt for batch title/merge suggestions.
func BuildSuggestMergeCandidatesPrompt(implementationSummaries string) string {
	return fmt.Sprintf(suggestMergeCandidatesPrompt, implementationSummaries)
}

// ParseSuggestImplementationOutput extracts the structured response for a single implementation.
func ParseSuggestImplementationOutput(raw string) (*SuggestImplementationOutput, error) {
	cleaned := extractJSONFromMarkdown(raw)
	var out SuggestImplementationOutput
	if err := json.Unmarshal([]byte(cleaned), &out); err != nil {
		return nil, fmt.Errorf("parse implementation suggestion JSON: %w", err)
	}
	out.Title = strings.TrimSpace(strings.SplitN(out.Title, "\n", 2)[0])
	if out.Title == "" {
		return nil, fmt.Errorf("LLM returned empty title")
	}
	out.Summary = strings.TrimSpace(out.Summary)
	return &out, nil
}

// ParseSuggestMergeCandidatesOutput extracts the batch title/merge response.
func ParseSuggestMergeCandidatesOutput(raw string) (*SuggestMergeCandidateOutput, error) {
	cleaned := extractJSONFromMarkdown(raw)
	var out SuggestMergeCandidateOutput
	if err := json.Unmarshal([]byte(cleaned), &out); err != nil {
		return nil, fmt.Errorf("parse merge candidates JSON: %w", err)
	}
	for i := range out.Titles {
		out.Titles[i].Title = strings.TrimSpace(out.Titles[i].Title)
	}
	return &out, nil
}

package implementations

import (
	"context"
	"fmt"
	"strings"

	"github.com/semanticash/cli/internal/llm"
	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
	"github.com/semanticash/cli/internal/util"
)

// SuggestResult holds the LLM-generated suggestions for a single implementation.
type SuggestResult struct {
	Title          string           `json:"title"`
	Summary        string           `json:"summary"`
	ReviewPriority []llm.ReviewItem `json:"review_priority,omitempty"`
	Provider       string           `json:"provider,omitempty"`
	Model          string           `json:"model,omitempty"`
}

// SuggestBatchResult holds title and merge suggestions across implementations.
type SuggestBatchResult struct {
	Titles    []llm.TitleSuggestion `json:"titles,omitempty"`
	Merges    []llm.MergeSuggestion `json:"merges,omitempty"`
	Provider  string                `json:"provider,omitempty"`
	Model     string                `json:"model,omitempty"`
	Truncated bool                  `json:"truncated,omitempty"` // true if input was capped
	Total     int                   `json:"total,omitempty"`     // total active+dormant count
	Analyzed  int                   `json:"analyzed,omitempty"`  // how many were sent to LLM
}

// GenerateTextFunc abstracts the LLM call for testing.
type GenerateTextFunc func(ctx context.Context, prompt string) (*llm.GenerateTextResult, error)

// SuggestService generates LLM-powered suggestions for implementations.
type SuggestService struct {
	GenerateText GenerateTextFunc
}

// NewSuggestService creates a SuggestService using the real LLM pipeline.
func NewSuggestService() *SuggestService {
	return &SuggestService{GenerateText: llm.GenerateText}
}

// SuggestForImplementation generates title, summary, and review priority
// for a single implementation.
func (s *SuggestService) SuggestForImplementation(ctx context.Context, implID string) (*SuggestResult, error) {
	detail, err := GetDetail(ctx, implID)
	if err != nil {
		return nil, err
	}

	prompt := buildSinglePrompt(detail)
	res, err := s.GenerateText(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("LLM call failed: %w", err)
	}

	parsed, err := llm.ParseSuggestImplementationOutput(res.Text)
	if err != nil {
		return nil, fmt.Errorf("parse LLM response: %w", err)
	}

	return &SuggestResult{
		Title:          parsed.Title,
		Summary:        parsed.Summary,
		ReviewPriority: parsed.ReviewPriority,
		Provider:       res.Provider,
		Model:          res.Model,
	}, nil
}

// batchLimit is the max implementations sent to the LLM in one batch.
// Bounded by prompt token budget, not arbitrary.
const batchLimit int64 = 100

// SuggestBatch generates title suggestions for untitled implementations
// and merge candidates across all active/dormant implementations.
func (s *SuggestService) SuggestBatch(ctx context.Context) (*SuggestBatchResult, error) {
	h, err := openGlobalDB(ctx)
	if err != nil {
		return nil, fmt.Errorf("open implementations db: %w", err)
	}
	defer func() { _ = impldb.Close(h) }()

	states := []string{"active", "dormant"}

	// Get total count to detect truncation.
	total, err := h.Queries.CountImplementationsByState(ctx, states)
	if err != nil {
		return nil, fmt.Errorf("count implementations: %w", err)
	}
	if total == 0 {
		return &SuggestBatchResult{}, nil
	}

	rows, err := h.Queries.ListImplementationsByState(ctx, impldbgen.ListImplementationsByStateParams{
		States: states,
		Limit:  batchLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("list implementations: %w", err)
	}

	analyzed := len(rows)
	truncated := total > int64(analyzed)

	summaries := buildBatchSummaries(ctx, h, rows)
	prompt := llm.BuildSuggestMergeCandidatesPrompt(summaries)

	res, err := s.GenerateText(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("LLM call failed: %w", err)
	}

	parsed, err := llm.ParseSuggestMergeCandidatesOutput(res.Text)
	if err != nil {
		return nil, fmt.Errorf("parse LLM response: %w", err)
	}

	return &SuggestBatchResult{
		Titles:    parsed.Titles,
		Merges:    parsed.Merges,
		Provider:  res.Provider,
		Model:     res.Model,
		Truncated: truncated,
		Total:     int(total),
		Analyzed:  analyzed,
	}, nil
}

// ApplyTitle writes a suggested title to an implementation.
func ApplyTitle(ctx context.Context, implID, title string) error {
	h, err := openGlobalDB(ctx)
	if err != nil {
		return fmt.Errorf("open implementations db: %w", err)
	}
	defer func() { _ = impldb.Close(h) }()

	fullID, err := resolveImplID(ctx, h, implID)
	if err != nil {
		return err
	}

	return h.Queries.UpdateImplementationTitle(ctx, impldbgen.UpdateImplementationTitleParams{
		Title:            impldb.NullStr(title),
		ImplementationID: fullID,
	})
}

// --- prompt construction helpers ---

func buildSinglePrompt(detail *ImplementationDetail) string {
	// Repos
	repoNames := make([]string, 0, len(detail.Repos))
	for _, r := range detail.Repos {
		repoNames = append(repoNames, fmt.Sprintf("%s (%s)", r.DisplayName, r.Role))
	}

	// Commits
	var commits strings.Builder
	for _, c := range detail.Commits {
		fmt.Fprintf(&commits, "  %s %s\n", c.DisplayName, c.CommitHash[:minLen(len(c.CommitHash), 7)])
	}
	if commits.Len() == 0 {
		commits.WriteString("  (none)\n")
	}

	// Timeline (last 50 entries to stay within prompt budget)
	var timeline strings.Builder
	start := 0
	if len(detail.Timeline) > 50 {
		start = len(detail.Timeline) - 50
	}
	for _, e := range detail.Timeline[start:] {
		prefix := "  "
		if e.CrossRepo {
			prefix = "→ "
		}
		fmt.Fprintf(&timeline, "  %s%s %s\n", prefix, e.RepoName, e.Summary)
	}
	if timeline.Len() == 0 {
		timeline.WriteString("  (no events)\n")
	}

	tokIn := compactTokens(detail.TotalTokensIn)
	tokOut := compactTokens(detail.TotalTokensOut)

	return llm.BuildSuggestImplementationPrompt(
		detail.State,
		strings.Join(repoNames, ", "),
		len(detail.Sessions),
		tokIn, tokOut,
		strings.TrimSpace(commits.String()),
		strings.TrimSpace(timeline.String()),
	)
}

func buildBatchSummaries(ctx context.Context, h *impldb.Handle, rows []impldbgen.ListImplementationsByStateRow) string {
	var b strings.Builder
	for _, r := range rows {
		id := util.ShortID(r.ImplementationID)
		title := "(untitled)"
		if r.Title.Valid && r.Title.String != "" {
			title = r.Title.String
		}

		repos, _ := h.Queries.ListImplementationRepos(ctx, r.ImplementationID)
		repoNames := make([]string, 0, len(repos))
		for _, rr := range repos {
			repoNames = append(repoNames, rr.DisplayName)
		}

		commits, _ := h.Queries.ListImplementationCommits(ctx, r.ImplementationID)
		commitHashes := make([]string, 0, len(commits))
		for _, c := range commits {
			commitHashes = append(commitHashes, c.CommitHash[:minLen(len(c.CommitHash), 7)])
		}

		fmt.Fprintf(&b, "- %s  %s  state=%s  repos=[%s]  commits=[%s]\n",
			id, title, r.State,
			strings.Join(repoNames, ", "),
			strings.Join(commitHashes, ", "))
	}
	return b.String()
}

func compactTokens(n int64) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

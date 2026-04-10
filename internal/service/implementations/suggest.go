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
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
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

// SuggestForImplementation generates a title and summary for a single implementation.
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
		Title:    parsed.Title,
		Summary:  parsed.Summary,
		Provider: res.Provider,
		Model:    res.Model,
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

// ApplySuggestion writes the suggested title and summary to an implementation.
func ApplySuggestion(ctx context.Context, implID, title, summary string) error {
	h, err := openGlobalDB(ctx)
	if err != nil {
		return fmt.Errorf("open implementations db: %w", err)
	}
	defer func() { _ = impldb.Close(h) }()

	fullID, err := resolveImplID(ctx, h, implID)
	if err != nil {
		return err
	}

	impl, err := h.Queries.GetImplementation(ctx, fullID)
	if err != nil {
		return fmt.Errorf("get implementation: %w", err)
	}

	metadata, err := implementationMetadataWithSummary(impl.MetadataJson, summary)
	if err != nil {
		return err
	}

	tx, err := h.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin implementation update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	qtx := h.Queries.WithTx(tx)
	if err := qtx.UpdateImplementationTitle(ctx, impldbgen.UpdateImplementationTitleParams{
		Title:            impldb.NullStr(title),
		ImplementationID: fullID,
	}); err != nil {
		return fmt.Errorf("update implementation title: %w", err)
	}
	if err := qtx.UpdateImplementationMetadata(ctx, impldbgen.UpdateImplementationMetadataParams{
		MetadataJson:     metadata,
		ImplementationID: fullID,
	}); err != nil {
		return fmt.Errorf("update implementation metadata: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit implementation update: %w", err)
	}
	return nil
}

// ApplyTitle writes only a suggested title to an implementation.
// Kept as a convenience wrapper for callers that do not need summary persistence.
func ApplyTitle(ctx context.Context, implID, title string) error {
	return ApplySuggestion(ctx, implID, title, "")
}

// --- prompt construction helpers ---

func buildSinglePrompt(detail *ImplementationDetail) string {
	repoNames := make([]string, 0, len(detail.Repos))
	for _, r := range detail.Repos {
		repoNames = append(repoNames, fmt.Sprintf("%s (%s)", r.DisplayName, r.Role))
	}

	startedIn := suggestStartedIn(detail)
	if startedIn == "" {
		startedIn = "(unknown)"
	}

	var commits strings.Builder
	for _, c := range detail.Commits {
		subject := strings.TrimSpace(c.Subject)
		if subject == "" {
			subject = "(no subject)"
		}
		fmt.Fprintf(&commits, "  %s %s %s\n",
			c.DisplayName,
			c.CommitHash[:minLen(len(c.CommitHash), 7)],
			subject)
	}
	if commits.Len() == 0 {
		commits.WriteString("  (none)\n")
	}

	var fileChanges strings.Builder
	for _, line := range suggestTopFileChanges(detail, 10) {
		fmt.Fprintf(&fileChanges, "  %s\n", line)
	}
	if fileChanges.Len() == 0 {
		fileChanges.WriteString("  (none)\n")
	}

	tokIn := compactTokens(detail.TotalTokensIn)
	tokOut := compactTokens(detail.TotalTokensOut)

	return llm.BuildSuggestImplementationPrompt(
		detail.State,
		startedIn,
		strings.Join(repoNames, ", "),
		len(detail.Sessions),
		tokIn, tokOut,
		strings.TrimSpace(commits.String()),
		strings.TrimSpace(fileChanges.String()),
	)
}

func suggestStartedIn(detail *ImplementationDetail) string {
	provider := ""
	if len(detail.Sessions) > 0 {
		provider = suggestProviderDisplayName(detail.Sessions[0].Provider)
	}
	for _, repo := range detail.Repos {
		if repo.Role == "origin" {
			if provider != "" {
				return fmt.Sprintf("%s (%s)", repo.DisplayName, provider)
			}
			return repo.DisplayName
		}
	}
	if len(detail.Repos) > 0 {
		if provider != "" {
			return fmt.Sprintf("%s (%s)", detail.Repos[0].DisplayName, provider)
		}
		return detail.Repos[0].DisplayName
	}
	return ""
}

func suggestTopFileChanges(detail *ImplementationDetail, limit int) []string {
	type item struct {
		repo string
		path string
		op   string
	}
	if limit <= 0 {
		limit = 10
	}
	seen := make(map[string]bool)
	items := make([]item, 0, limit)
	for i := len(detail.Timeline) - 1; i >= 0; i-- {
		entry := detail.Timeline[i]
		if entry.Kind == "commit" || strings.TrimSpace(entry.FilePath) == "" {
			continue
		}
		if suggestIsInternalPath(entry.FilePath) {
			continue
		}
		key := entry.RepoName + "|" + entry.FilePath + "|" + entry.FileOp
		if seen[key] {
			continue
		}
		seen[key] = true
		items = append(items, item{repo: entry.RepoName, path: entry.FilePath, op: entry.FileOp})
		if len(items) == limit {
			break
		}
	}

	lines := make([]string, 0, len(items))
	for _, it := range items {
		suffix := ""
		if it.op != "" {
			suffix = " (" + it.op + ")"
		}
		lines = append(lines, fmt.Sprintf("%s %s%s", it.repo, it.path, suffix))
	}
	return lines
}

func suggestIsInternalPath(path string) bool {
	lower := strings.ToLower(strings.TrimSpace(path))
	return strings.HasPrefix(lower, ".claude/") ||
		strings.HasPrefix(lower, ".cursor/") ||
		strings.HasPrefix(lower, ".gemini/") ||
		strings.HasPrefix(lower, ".semantica/") ||
		strings.HasPrefix(lower, ".git/") ||
		strings.HasPrefix(lower, ".kiro/") ||
		lower == ".gitignore"
}

func suggestProviderDisplayName(provider string) string {
	switch provider {
	case "claude_code":
		return "Claude"
	case "cursor":
		return "Cursor"
	case "gemini_cli":
		return "Gemini"
	case "copilot":
		return "Copilot"
	default:
		return ""
	}
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

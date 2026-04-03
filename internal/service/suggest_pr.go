package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/llm"
)

// GenerateTextFunc matches llm.GenerateText and can be replaced in tests.
type GenerateTextFunc func(ctx context.Context, prompt string) (*llm.GenerateTextResult, error)

type SuggestPRService struct {
	GenerateText GenerateTextFunc
}

func NewSuggestPRService() *SuggestPRService {
	return &SuggestPRService{GenerateText: llm.GenerateText}
}

type SuggestPRInput struct {
	RepoPath string
	Base     string // base branch; empty means auto-detect
}

type SuggestPRResult struct {
	Title    string `json:"title"`
	Body     string `json:"body"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Dirty    bool   `json:"dirty,omitempty"` // true if working tree had uncommitted changes
}

const maxCommitSubjects = 30

func (s *SuggestPRService) SuggestPR(ctx context.Context, in SuggestPRInput) (*SuggestPRResult, error) {
	repoPath := in.RepoPath
	if repoPath == "" {
		repoPath = "."
	}

	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return nil, err
	}
	repoRoot := repo.Root()

	// Dirty worktree warning (returned in result, not blocking).
	dirty, _ := repo.IsDirty(ctx)

	// Determine base ref.
	base := in.Base
	if base == "" {
		base, err = repo.DefaultBaseRef(ctx)
		if err != nil {
			return nil, err
		}
	}

	// Ensure we're on a branch.
	branch, err := repo.CurrentBranch(ctx)
	if err != nil {
		return nil, err
	}
	if branch == "HEAD" {
		return nil, fmt.Errorf("detached HEAD; check out a branch first")
	}

	// Get the diff.
	diffBytes, err := repo.DiffBetween(ctx, base, "HEAD")
	if err != nil {
		return nil, err
	}
	if len(diffBytes) == 0 {
		return nil, fmt.Errorf("no changes between %s and %s", base, branch)
	}

	// Get commit subjects (no hashes).
	subjects, err := repo.CommitSubjectsBetween(ctx, base, "HEAD", maxCommitSubjects)
	if err != nil {
		return nil, fmt.Errorf("commit log: %w", err)
	}
	subjectBlock := strings.Join(subjects, "\n")

	// Try to read PR template.
	prTemplate := readPRTemplate(repoRoot)

	// Build prompt and call LLM.
	prompt := llm.BuildSuggestPRPrompt(string(diffBytes), subjectBlock, prTemplate)

	res, err := s.GenerateText(ctx, prompt)
	if err != nil {
		return nil, err
	}

	parsed, err := llm.ParseSuggestPROutput(res.Text)
	if err != nil {
		return nil, err
	}

	return &SuggestPRResult{
		Title:    parsed.Title,
		Body:     parsed.Body,
		Provider: res.Provider,
		Model:    res.Model,
		Dirty:    dirty,
	}, nil
}

// readPRTemplate searches for a pull request template following GitHub's
// resolution order: .github/, docs/, repo root - checking both single-file
// templates and PULL_REQUEST_TEMPLATE/ subdirectories (first .md wins).
// Returns empty string if none found. Strips HTML comments.
func readPRTemplate(repoRoot string) string {
	dirs := []string{
		filepath.Join(repoRoot, ".github"),
		filepath.Join(repoRoot, "docs"),
		repoRoot,
	}
	names := []string{"pull_request_template.md", "PULL_REQUEST_TEMPLATE.md"}

	var candidates []string
	for _, dir := range dirs {
		for _, name := range names {
			candidates = append(candidates, filepath.Join(dir, name))
		}
		tmplDir := filepath.Join(dir, "PULL_REQUEST_TEMPLATE")
		if entries, err := os.ReadDir(tmplDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
					candidates = append(candidates, filepath.Join(tmplDir, e.Name()))
					break // use the first .md found
				}
			}
		}
	}

	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		return llm.StripHTMLComments(string(data))
	}
	return ""
}

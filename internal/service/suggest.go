package service

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/llm"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

type SuggestService struct{}

func NewSuggestService() *SuggestService { return &SuggestService{} }

type SuggestInput struct {
	RepoPath string
}

type SuggestResult struct {
	Message  string `json:"message"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

func (s *SuggestService) Suggest(ctx context.Context, in SuggestInput) (*SuggestResult, error) {
	repoPath := in.RepoPath
	if repoPath == "" {
		repoPath = "."
	}

	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return nil, err
	}
	repoRoot := repo.Root()

	// Get all uncommitted changes: staged, unstaged, and untracked.
	diffBytes, err := repo.DiffAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("diff: %w", err)
	}
	if len(diffBytes) == 0 {
		return nil, fmt.Errorf("no changes to commit")
	}

	// Best-effort: load recent session transcript for context.
	transcript := s.recentTranscript(ctx, repoRoot)

	prompt := llm.BuildCommitMsgPrompt(transcript, string(diffBytes))

	res, err := llm.GenerateText(ctx, prompt)
	if err != nil {
		return nil, err
	}

	return &SuggestResult{
		Message:  sanitizeCommitMessage(res.Text),
		Provider: res.Provider,
		Model:    res.Model,
	}, nil
}

func sanitizeCommitMessage(msg string) string {
	msg = strings.TrimSpace(msg)
	msg = strings.ReplaceAll(msg, "```", "")
	msg = strings.ReplaceAll(msg, "`", "")
	msg = strings.Join(strings.Fields(msg), " ")
	if msg == "" {
		return ""
	}
	return limitCommitMessageSentences(msg, 2)
}

var commitSentenceBoundary = regexp.MustCompile(`([.!?])\s+`)

func limitCommitMessageSentences(msg string, maxSentences int) string {
	if maxSentences <= 0 || msg == "" {
		return ""
	}

	matches := commitSentenceBoundary.FindAllStringSubmatchIndex(msg, -1)
	if len(matches) < maxSentences {
		return strings.TrimSpace(msg)
	}

	end := matches[maxSentences-1][3]
	return strings.TrimSpace(msg[:end])
}

// recentTranscript loads transcript events from the most recent session
// window (last 30 minutes). Returns nil if unavailable.
func (s *SuggestService) recentTranscript(ctx context.Context, repoRoot string) []llm.TranscriptEntry {
	semDir := filepath.Join(repoRoot, ".semantica")
	if !util.IsEnabled(semDir) {
		return nil
	}
	dbPath := filepath.Join(semDir, "lineage.db")

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 200 * time.Millisecond,
		Synchronous: "NORMAL",
	})
	if err != nil {
		return nil
	}
	defer func() { _ = sqlstore.Close(h) }()

	repoRow, err := h.Queries.GetRepositoryByRootPath(ctx, repoRoot)
	if err != nil {
		return nil
	}

	sinceTs := time.Now().Add(-30 * time.Minute).UnixMilli()
	events, err := h.Queries.ListTranscriptEvents(ctx, sqldb.ListTranscriptEventsParams{
		RepositoryID: repoRow.RepositoryID,
		AfterTs:      sinceTs,
		UntilTs:      time.Now().UnixMilli(),
	})
	if err != nil || len(events) == 0 {
		return nil
	}

	var entries []llm.TranscriptEntry
	for _, e := range events {
		role := nullStr(e.Role)
		summary := nullStr(e.Summary)
		toolUses := nullStr(e.ToolUses)

		entry := llm.TranscriptEntry{Role: role, Summary: summary}

		if toolUses != "" {
			var newFmt struct {
				Tools []struct {
					Name     string `json:"name"`
					FilePath string `json:"file_path"`
				} `json:"tools"`
			}
			if err := json.Unmarshal([]byte(toolUses), &newFmt); err == nil && len(newFmt.Tools) > 0 {
				entry.ToolName = newFmt.Tools[0].Name
				entry.FilePath = newFmt.Tools[0].FilePath
			}
		}

		entries = append(entries, entry)
	}

	// Cap at 100 entries to keep the prompt reasonable.
	if len(entries) > 100 {
		entries = entries[len(entries)-100:]
	}

	return entries
}

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/semanticash/cli/internal/auth"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/redact"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	"github.com/semanticash/cli/internal/util"
	"github.com/semanticash/cli/internal/version"
)

// providerDetail is the per-provider breakdown in the push payload.
type providerDetail struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	AILines  int    `json:"ai_lines"`
}

// remotePushPayload is the JSON body POSTed to the remote attribution endpoint.
// NOTE: remote_url comes from `git remote get-url origin` which may be SSH or
// HTTPS. The backend must canonicalize this (e.g., normalize SSH and HTTPS,
// strip/add .git suffix) before using it as a repository identity key.
type remotePushPayload struct {
	RemoteURL        string                 `json:"remote_url"`
	RepoProvider     string                 `json:"repo_provider,omitempty"`
	Branch           string                 `json:"branch,omitempty"`
	CommitHash       string                 `json:"commit_hash"`
	CommitSubject    string                 `json:"commit_subject,omitempty"`
	CheckpointID     string                 `json:"checkpoint_id,omitempty"`
	AIExactLines     int                    `json:"ai_exact_lines"`
	AIFormattedLines int                    `json:"ai_formatted_lines"`
	AIModifiedLines  int                    `json:"ai_modified_lines"`
	AILines          int                    `json:"ai_lines"`
	HumanLines       int                    `json:"human_lines"`
	TotalLines       int                    `json:"total_lines"`
	FilesTotal       int                    `json:"files_total"`
	FilesAITouched   int                    `json:"files_ai_touched"`
	Files            []FileAttribution      `json:"files"`
	Diagnostics      AttributionDiagnostics `json:"diagnostics"`
	SessionCount     int                    `json:"session_count,omitempty"`
	Providers        []string               `json:"providers,omitempty"`
	ProviderDetails  []providerDetail       `json:"provider_details,omitempty"`
	PlaybookJSON     json.RawMessage        `json:"playbook_json,omitempty"`
	Evidence         string                 `json:"evidence,omitempty"`
	FallbackCount    int                    `json:"fallback_count,omitempty"`
	CLIVersion       string                 `json:"cli_version,omitempty"`
	AttrVersion      string                 `json:"attribution_version"`
	PushedAt         int64                  `json:"pushed_at"`
}

// PushAction classifies the outcome of a push attempt.
type PushAction string

const (
	PushUploaded PushAction = "uploaded" // remote upsert succeeded
	PushRetry    PushAction = "retry"    // transient remote/auth failure
	PushSkip     PushAction = "skip"     // local failure or permanently non-retryable
)

// PushResult is the structured outcome of tryPushAttribution.
type PushResult struct {
	CommitHash   string
	CheckpointID string
	Action       PushAction
	AIPercentage float64
	Err          error
}

// buildPushPayload constructs the remote attribution payload by combining
// the attribution result with git metadata and checkpoint-scoped enrichment
// from the local DB.
func buildPushPayload(ctx context.Context, h *sqlstore.Handle, result *AttributionResult, remoteURL, branch, commitHash, subject, checkpointID string) remotePushPayload {
	var sessionCount int
	var providers []string
	stats, statsErr := h.Queries.GetCheckpointStats(ctx, checkpointID)
	if statsErr == nil {
		sessionCount = int(stats.SessionCount)
	}
	providerList, provErr := h.Queries.ListProvidersByCheckpoint(ctx, checkpointID)
	if provErr == nil {
		providers = providerList
	}

	var playbookJSON json.RawMessage
	row, err := h.Queries.GetCheckpointSummary(ctx, checkpointID)
	if err == nil && row.SummaryJson.Valid {
		playbookJSON = json.RawMessage(row.SummaryJson.String)
	}

	var details []providerDetail
	for _, pa := range result.ProviderDetails {
		details = append(details, providerDetail(pa))
	}

	return remotePushPayload{
		RemoteURL:        remoteURL,
		RepoProvider:     git.ProviderFromRemoteURL(remoteURL),
		Branch:           branch,
		CommitHash:       commitHash,
		CommitSubject:    subject,
		CheckpointID:     checkpointID,
		AIExactLines:     result.AIExactLines,
		AIFormattedLines: result.AIFormattedLines,
		AIModifiedLines:  result.AIModifiedLines,
		AILines:          result.AILines,
		HumanLines:       result.HumanLines,
		TotalLines:       result.TotalLines,
		FilesTotal:       result.FilesTotal,
		FilesAITouched:   result.FilesAITouched,
		Files:            result.Files,
		Diagnostics:      result.Diagnostics,
		SessionCount:     sessionCount,
		Providers:        providers,
		ProviderDetails:  details,
		PlaybookJSON:     playbookJSON,
		Evidence:         result.Evidence,
		FallbackCount:    result.FallbackCount,
		CLIVersion:       version.Version,
		AttrVersion:      "v1",
		PushedAt:         time.Now().UnixMilli(),
	}
}

// tryPushAttribution computes attribution for the commit and POSTs it to the
// remote endpoint. Returns a structured result so callers can decide how to
// handle the outcome (log-only, advance cursor, retry later, etc.).
func tryPushAttribution(ctx context.Context, repo *git.Repo, h *sqlstore.Handle, commitHash, checkpointID string) PushResult {
	endpoint := auth.EffectiveEndpoint()
	attrSvc := NewAttributionService()
	result, err := attrSvc.AttributeCommit(ctx, AttributionInput{
		RepoPath:   repo.Root(),
		CommitHash: commitHash,
	})
	if err != nil {
		return PushResult{CommitHash: commitHash, CheckpointID: checkpointID, Action: PushSkip, Err: fmt.Errorf("attribution failed: %w", err)}
	}

	branch, _ := repo.CurrentBranch(ctx)
	remoteURL, _ := repo.RemoteURL(ctx)
	subject, _ := repo.CommitSubject(ctx, commitHash)

	payload := buildPushPayload(ctx, h, result, remoteURL, branch, commitHash, subject, checkpointID)

	payload.RemoteURL = redact.SanitizeURL(payload.RemoteURL)
	if err := redactPushPayload(&payload); err != nil {
		return PushResult{CommitHash: commitHash, CheckpointID: checkpointID, Action: PushSkip, Err: fmt.Errorf("redaction failed: %w", err)}
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return PushResult{CommitHash: commitHash, CheckpointID: checkpointID, Action: PushSkip, Err: fmt.Errorf("marshal failed: %w", err)}
	}

	pushCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(pushCtx, "POST", endpoint+"/v1/attribution", bytes.NewReader(body))
	if err != nil {
		return PushResult{CommitHash: commitHash, CheckpointID: checkpointID, Action: PushSkip, Err: fmt.Errorf("build request: %w", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", version.UserAgent())
	token, tokenErr := auth.AccessToken(ctx)
	if tokenErr != nil {
		return PushResult{CommitHash: commitHash, CheckpointID: checkpointID, Action: PushRetry, Err: fmt.Errorf("auth: %w", tokenErr)}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return PushResult{CommitHash: commitHash, CheckpointID: checkpointID, Action: PushRetry, Err: fmt.Errorf("request: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized && token != "" && !auth.IsAPIKeyAuth() {
		_ = resp.Body.Close()
		refreshed, refreshErr := auth.ForceRefresh(ctx)
		if refreshErr != nil {
			return PushResult{CommitHash: commitHash, CheckpointID: checkpointID, Action: PushRetry, Err: fmt.Errorf("refresh after 401: %w", refreshErr)}
		}
		retryReq, _ := http.NewRequestWithContext(pushCtx, "POST", endpoint+"/v1/attribution", bytes.NewReader(body))
		retryReq.Header.Set("Content-Type", "application/json")
		retryReq.Header.Set("User-Agent", version.UserAgent())
		retryReq.Header.Set("Authorization", "Bearer "+refreshed)
		resp, err = http.DefaultClient.Do(retryReq)
		if err != nil {
			return PushResult{CommitHash: commitHash, CheckpointID: checkpointID, Action: PushRetry, Err: fmt.Errorf("retry request: %w", err)}
		}
		defer func() { _ = resp.Body.Close() }()
	}

	if resp.StatusCode == 404 {
		return PushResult{CommitHash: commitHash, CheckpointID: checkpointID, Action: PushSkip, Err: fmt.Errorf("repository not connected")}
	}
	if resp.StatusCode >= 300 {
		return PushResult{CommitHash: commitHash, CheckpointID: checkpointID, Action: PushRetry, Err: fmt.Errorf("server returned %d", resp.StatusCode)}
	}

	return PushResult{
		CommitHash:   commitHash,
		CheckpointID: checkpointID,
		Action:       PushUploaded,
		AIPercentage: result.AIPercentage,
	}
}

// pushAttribution is the log-only wrapper used by the worker call site.
func pushAttribution(ctx context.Context, repo *git.Repo, h *sqlstore.Handle, commitHash, checkpointID string) PushResult {
	r := tryPushAttribution(ctx, repo, h, commitHash, checkpointID)
	switch r.Action {
	case PushUploaded:
		wlog("worker: push-remote: pushed attribution for %s (%.0f%% AI)\n", util.ShortID(commitHash), r.AIPercentage)
	case PushSkip:
		wlog("worker: push-remote: %v\n", r.Err)
	case PushRetry:
		wlog("worker: push-remote: %v\n", r.Err)
	}
	return r
}

// handlePushRetryBackfill extends the backfill cutoff when a live push fails
// with a retryable error, so a later checkpoint drain can retry it.
func handlePushRetryBackfill(ctx context.Context, wctx *workerContext, commitHash string) {
	s, err := util.ReadSettings(wctx.semDir)
	if err != nil {
		wlog("worker: backfill: read settings: %v\n", err)
		return
	}
	if s.ConnectedRepoID == "" {
		wlog("worker: backfill: connected repo id missing after live retry for %s\n", util.ShortID(commitHash))
		return
	}
	cl, err := wctx.h.Queries.GetCommitLinkByCommitHash(ctx, commitHash)
	if err != nil {
		wlog("worker: backfill: load commit link for %s: %v\n", util.ShortID(commitHash), err)
		return
	}
	if err := ExtendBackfillCutoff(ctx, wctx.h, s.ConnectedRepoID, cl.RepositoryID, cl.CommitHash, cl.LinkedAt); err != nil {
		wlog("worker: backfill: extend cutoff for %s: %v\n", util.ShortID(commitHash), err)
	}
}

// drainBackfillFromWorker drains a small batch of historical attribution
// after a connected checkpoint completes. Best-effort: logs only.
func drainBackfillFromWorker(ctx context.Context, repoRoot, semDir string) {
	s, err := util.ReadSettings(semDir)
	if err != nil || s.ConnectedRepoID == "" {
		return
	}

	const workerBackfillBatchSize = 5
	result := DrainBackfillBatch(ctx, repoRoot, s.ConnectedRepoID, workerBackfillBatchSize)

	if result.Uploaded > 0 {
		wlog("worker: backfill: pushed %d historical commit(s)\n", result.Uploaded)
	}
	if result.Skipped > 0 {
		wlog("worker: backfill: skipped %d commit(s)\n", result.Skipped)
	}
	if result.Failed {
		wlog("worker: backfill: paused: %s\n", result.Reason)
	}
	if result.Done && (result.Uploaded > 0 || result.Skipped > 0) {
		wlog("worker: backfill: historical attribution complete\n")
	}
}

// RePushAttribution re-pushes attribution for a commit to the remote endpoint.
// Called after auto-playbook saves a summary so the backend gets the enriched
// playbook_summary and can rematerialize PR comments.
func RePushAttribution(ctx context.Context, repoRoot, commitHash, checkpointID string) {
	repo, err := git.OpenRepo(repoRoot)
	if err != nil {
		wlog("worker: re-push: open repo: %v\n", err)
		return
	}

	semDir := filepath.Join(repo.Root(), ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		wlog("worker: re-push: open db: %v\n", err)
		return
	}
	defer func() { _ = sqlstore.Close(h) }()

	pushAttribution(ctx, repo, h, commitHash, checkpointID)
	wlog("worker: re-push: sent enriched attribution for %s\n", util.ShortID(commitHash))
}

// redactPushPayload redacts free-text fields in the outbound push payload.
func redactPushPayload(p *remotePushPayload) error {
	var err error
	if p.CommitSubject != "" {
		p.CommitSubject, err = redact.String(p.CommitSubject)
		if err != nil {
			return err
		}
	}
	if len(p.PlaybookJSON) > 0 {
		redacted, redactErr := redact.Bytes(p.PlaybookJSON)
		if redactErr != nil {
			return redactErr
		}
		p.PlaybookJSON = redacted
	}
	return nil
}

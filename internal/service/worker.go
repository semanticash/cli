package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/semanticash/cli/internal/auth"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/provenance"
	"github.com/semanticash/cli/internal/redact"
	"github.com/semanticash/cli/internal/service/implementations"
	"github.com/semanticash/cli/internal/store/blobs"
	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
	"github.com/semanticash/cli/internal/version"

	// Register hook providers via init().
	_ "github.com/semanticash/cli/internal/hooks/claude"
	_ "github.com/semanticash/cli/internal/hooks/copilot"
	_ "github.com/semanticash/cli/internal/hooks/cursor"
	_ "github.com/semanticash/cli/internal/hooks/gemini"
	_ "github.com/semanticash/cli/internal/hooks/kirocli"
	_ "github.com/semanticash/cli/internal/hooks/kiroide"
)

type WorkerService struct{}

func NewWorkerService() *WorkerService { return &WorkerService{} }

type WorkerInput struct {
	CheckpointID string
	CommitHash   string // optional, for logging
	RepoRoot     string
}

// workerContext carries the shared infrastructure handles opened during
// checkpoint preparation. Passed to stage functions to avoid threading
// many parameters through every call.
type workerContext struct {
	h         *sqlstore.Handle
	blobStore *blobs.Store
	repo      *git.Repo
	cp        sqldb.Checkpoint
	semDir    string
}

func (wc *workerContext) close() { _ = sqlstore.Close(wc.h) }

// prepareResult is the outcome of prepareCheckpoint.
type prepareResult struct {
	wctx *workerContext
	skip bool // true when checkpoint is already complete/failed, not found, or semantica disabled
}

func wlog(format string, args ...any) {
	ts := time.Now().Format(time.RFC3339)
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "%s  %s", ts, msg)
}

// prepareCheckpoint opens the DB, validates the checkpoint is pending,
// initializes the blob store, and opens the git repo. Returns skip=true
// when the worker should exit early without error.
func prepareCheckpoint(ctx context.Context, in WorkerInput) (prepareResult, error) {
	semDir := filepath.Join(in.RepoRoot, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")
	objectsDir := filepath.Join(semDir, "objects")

	if !util.IsEnabled(semDir) {
		return prepareResult{skip: true}, nil
	}

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 5 * time.Second,
		Synchronous: "NORMAL",
	})
	if err != nil {
		return prepareResult{}, fmt.Errorf("open db: %w", err)
	}

	cp, err := h.Queries.GetCheckpointByID(ctx, in.CheckpointID)
	if err != nil {
		_ = sqlstore.Close(h)
		if errors.Is(err, sql.ErrNoRows) {
			wlog("worker: checkpoint %s not found, skipping\n", in.CheckpointID)
			return prepareResult{skip: true}, nil
		}
		return prepareResult{}, fmt.Errorf("get checkpoint: %w", err)
	}
	switch cp.Status {
	case "complete":
		_ = sqlstore.Close(h)
		wlog("worker: checkpoint %s already complete, skipping\n", in.CheckpointID)
		return prepareResult{skip: true}, nil
	case "failed":
		_ = sqlstore.Close(h)
		wlog("worker: checkpoint %s marked failed, skipping\n", in.CheckpointID)
		return prepareResult{skip: true}, nil
	}

	blobStore, err := blobs.NewStore(objectsDir)
	if err != nil {
		failCheckpoint(ctx, h, in.CheckpointID)
		_ = sqlstore.Close(h)
		return prepareResult{}, fmt.Errorf("init blob store: %w", err)
	}

	repo, err := git.OpenRepo(in.RepoRoot)
	if err != nil {
		failCheckpoint(ctx, h, in.CheckpointID)
		_ = sqlstore.Close(h)
		return prepareResult{}, fmt.Errorf("open repo: %w", err)
	}

	return prepareResult{
		wctx: &workerContext{
			h:         h,
			blobStore: blobStore,
			repo:      repo,
			cp:        cp,
			semDir:    semDir,
		},
	}, nil
}

func (s *WorkerService) Run(ctx context.Context, in WorkerInput) error {
	prep, err := prepareCheckpoint(ctx, in)
	if err != nil {
		return err
	}
	if prep.skip {
		return nil
	}
	wctx := prep.wctx
	defer wctx.close()

	// Reconciliation must run before manifest/checkpoint completion so
	// recovered events are included in this checkpoint.
	reconcileActiveSessions(ctx)

	// Reconcile implementation observations (best-effort).
	// Creates implementations.db on first call, processes pending observations.
	// Commit attachment happens later, after session_checkpoints are written.
	reconcileImplementations(ctx, in.RepoRoot)

	h := wctx.h
	repo := wctx.repo
	blobStore := wctx.blobStore
	cp := wctx.cp
	semDir := wctx.semDir

	paths, err := repo.ListFilesFromGit(ctx)
	if err != nil {
		failCheckpoint(ctx, h, in.CheckpointID)
		return fmt.Errorf("list files: %w", err)
	}

	// Load previous manifest for incremental building and later diff counting.
	prevManifest := loadPreviousManifest(ctx, h, blobStore, cp.RepositoryID, cp.CreatedAt)

	mr, err := blobs.BuildManifest(ctx, blobStore, in.RepoRoot, paths, repo.ReadFile, prevManifest.files)
	if err != nil {
		failCheckpoint(ctx, h, in.CheckpointID)
		return err
	}
	manifestHash := mr.ManifestHash
	totalBytes := mr.TotalBytes
	manifest := mr.Manifest

	// For session linking / file counting, use GetPreviousCompletedCheckpoint.
	var afterTs int64
	prev, prevErr := h.Queries.GetPreviousCompletedCheckpoint(ctx, sqldb.GetPreviousCompletedCheckpointParams{
		RepositoryID: cp.RepositoryID,
		CreatedAt:    cp.CreatedAt,
	})
	if prevErr == nil {
		afterTs = prev.CreatedAt
	}

	// For attribution window, use GetPreviousCommitLinkedCheckpoint
	// (consistent with hook_commit_msg.go and AttributeCommit).
	var attrAfterTs int64
	var prevCLPtr *sqldb.Checkpoint
	prevCL, prevCLErr := h.Queries.GetPreviousCommitLinkedCheckpoint(ctx, sqldb.GetPreviousCommitLinkedCheckpointParams{
		RepositoryID: cp.RepositoryID,
		CreatedAt:    cp.CreatedAt,
	})
	if prevCLErr == nil {
		attrAfterTs = prevCL.CreatedAt
		prevCLPtr = &prevCL
	}

	windowSessions, sessErr := h.Queries.ListSessionsWithEventsInWindow(ctx, sqldb.ListSessionsWithEventsInWindowParams{
		RepositoryID: cp.RepositoryID,
		AfterTs:      afterTs,
		UpToTs:       cp.CreatedAt,
	})
	if sessErr != nil {
		wlog("worker: list sessions in window: %v\n", sessErr)
	}

	seen := make(map[string]bool, len(windowSessions))
	for _, sid := range windowSessions {
		seen[sid] = true
	}

	for sid := range seen {
		if err := h.Queries.InsertSessionCheckpoint(ctx, sqldb.InsertSessionCheckpointParams{
			SessionID:    sid,
			CheckpointID: in.CheckpointID,
		}); err != nil {
			wlog("worker: link session %s to checkpoint: %v\n", sid, err)
		}
	}

	// Attach this commit to its implementation now that session_checkpoints
	// exist. AttachCommit depends on commit_links (written by post-commit hook
	// before the worker) and session_checkpoints (written just above).
	if in.CommitHash != "" {
		handleImplementationPostCommit(ctx, semDir, in.RepoRoot, in.CommitHash)
	}

	filesChanged := countChangedFiles(prevManifest, manifest.Files)

	if err := h.Queries.UpsertCheckpointStats(ctx, sqldb.UpsertCheckpointStatsParams{
		CheckpointID: in.CheckpointID,
		SessionCount: int64(len(seen)),
		FilesChanged: filesChanged,
	}); err != nil {
		wlog("worker: upsert checkpoint stats: %v\n", err)
	}

	if in.CommitHash != "" {
		diffBytes, diffErr := repo.DiffForCommit(ctx, in.CommitHash)
		if diffErr == nil && len(diffBytes) > 0 {
			cfr, attrErr := attributeWithCarryForward(ctx, h, blobStore, diffBytes, ComputeAIPercentInput{
				RepoRoot: in.RepoRoot,
				RepoID:   cp.RepositoryID,
				AfterTs:  attrAfterTs,
				UpToTs:   cp.CreatedAt,
			}, prevCLPtr, semDir)
			if attrErr == nil {
				if err := h.Queries.UpdateCheckpointAIPercentage(ctx, sqldb.UpdateCheckpointAIPercentageParams{
					AiPercentage: cfr.result.Percent,
					CheckpointID: in.CheckpointID,
				}); err != nil {
					wlog("worker: update AI percentage: %v\n", err)
				}
				wlog("worker: AI attribution: %.0f%%\n", cfr.result.Percent)
			}
		}
	}

	// Mark checkpoint complete only after all derived data (session links,
	// stats, AI percentage) has been written. This ensures readers never
	// observe a "complete" checkpoint with missing enrichment.
	if err := h.Queries.CompleteCheckpoint(ctx, sqldb.CompleteCheckpointParams{
		ManifestHash: sql.NullString{String: manifestHash, Valid: true},
		SizeBytes:    sql.NullInt64{Int64: totalBytes, Valid: true},
		CompletedAt:  sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true},
		CheckpointID: in.CheckpointID,
	}); err != nil {
		failCheckpoint(ctx, h, in.CheckpointID)
		return fmt.Errorf("complete checkpoint: %w", err)
	}

	wlog("worker: checkpoint %s complete (%d files, %d changed, %d bytes, commit %s)\n",
		in.CheckpointID, len(paths), filesChanged, totalBytes, in.CommitHash)

	// Auto-playbook runs asynchronously and updates the remote view later.
	if in.CommitHash != "" && util.IsPlaybookEnabled(semDir) {
		spawnAutoPlaybook(semDir, in.CheckpointID, in.CommitHash, in.RepoRoot)
	}

	// Sync provenance on checkpoint completion (independent of commit).
	// The checkpoint's created_at serves as the watermark - only sync turns
	// packaged at or before this checkpoint.
	if util.IsConnected(semDir) {
		syncProvenance(ctx, in.RepoRoot, cp.CreatedAt)
	}

	livePushRetried := false
	if in.CommitHash != "" && util.IsConnected(semDir) {
		pr := pushAttribution(ctx, repo, h, in.CommitHash, in.CheckpointID)

		// If the live push failed with a retryable error, fold it into the
		// backfill state so a later checkpoint drain can retry it.
		if pr.Action == PushRetry {
			livePushRetried = true
			s, settingsErr := util.ReadSettings(semDir)
			if settingsErr != nil {
				wlog("worker: backfill: read settings: %v\n", settingsErr)
			} else if s.ConnectedRepoID == "" {
				wlog("worker: backfill: connected repo id missing after live retry for %s\n", util.ShortID(in.CommitHash))
			} else {
				cl, clErr := h.Queries.GetCommitLinkByCommitHash(ctx, in.CommitHash)
				if clErr != nil {
					wlog("worker: backfill: load commit link for %s: %v\n", util.ShortID(in.CommitHash), clErr)
				} else if err := ExtendBackfillCutoff(ctx, h, s.ConnectedRepoID, cl.RepositoryID, cl.CommitHash, cl.LinkedAt); err != nil {
					wlog("worker: backfill: extend cutoff for %s: %v\n", util.ShortID(in.CommitHash), err)
				}
			}
		}
	}

	// Drain a small batch of historical attribution if a backfill is pending.
	// Skip when the live push just failed - the same transient issue would
	// likely fail the drain too, wasting a retry attempt.
	if util.IsConnected(semDir) && !livePushRetried {
		drainBackfillFromWorker(ctx, in.RepoRoot, semDir)
	}

	return nil
}

// reconcileActiveSessions flushes only sessions that still have capture state.
func reconcileActiveSessions(ctx context.Context) {
	states, err := hooks.LoadActiveCaptureStates()
	if err != nil || len(states) == 0 {
		return
	}

	registryPath, err := broker.DefaultRegistryPath()
	if err != nil {
		return
	}
	bh, err := broker.Open(ctx, registryPath)
	if err != nil {
		return
	}
	defer func() { _ = broker.Close(bh) }()

	var blobStore *blobs.Store
	if objDir, err := broker.GlobalObjectsDir(); err == nil {
		if bs, err := blobs.NewStore(objDir); err != nil {
			wlog("worker: reconcile: global blob store: %v (attribution will degrade)\n", err)
		} else {
			blobStore = bs
		}
	}

	for _, state := range states {
		provider := hooks.GetProvider(state.Provider)
		if provider == nil {
			continue
		}
		event := &hooks.Event{
			SessionID:     state.SessionID,
			TranscriptRef: state.TranscriptRef,
			Timestamp:     time.Now().UnixMilli(),
		}
		if err := hooks.CaptureAndRoute(ctx, provider, event, bh, blobStore); err != nil {
			wlog("worker: reconcile %s/%s: %v\n", state.Provider, state.SessionID, err)
		}
	}
}

// prevManifestResult holds the result of loading the previous manifest.
type prevManifestResult struct {
	files  []blobs.ManifestFile
	exists bool
	ok     bool
}

// loadPreviousManifest returns the previous completed manifest when available.
func loadPreviousManifest(ctx context.Context, h *sqlstore.Handle, bs *blobs.Store, repoID string, cpCreatedAt int64) prevManifestResult {
	prev, err := h.Queries.GetPreviousCompletedCheckpoint(ctx, sqldb.GetPreviousCompletedCheckpointParams{
		RepositoryID: repoID,
		CreatedAt:    cpCreatedAt,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return prevManifestResult{}
		}
		wlog("worker: get previous checkpoint: %v\n", err)
		return prevManifestResult{exists: true}
	}

	if !prev.ManifestHash.Valid || prev.ManifestHash.String == "" {
		return prevManifestResult{exists: true}
	}

	rawManifest, err := bs.Get(ctx, prev.ManifestHash.String)
	if err != nil {
		wlog("worker: load previous manifest: %v\n", err)
		return prevManifestResult{exists: true}
	}

	var prevManifest blobs.Manifest
	if err := json.Unmarshal(rawManifest, &prevManifest); err != nil {
		wlog("worker: unmarshal previous manifest: %v\n", err)
		return prevManifestResult{exists: true}
	}

	return prevManifestResult{files: prevManifest.Files, exists: true, ok: true}
}

// countChangedFiles compares current files to the previous manifest when one
// is available.
func countChangedFiles(prev prevManifestResult, currentFiles []blobs.ManifestFile) int64 {
	if !prev.exists {
		return int64(len(currentFiles))
	}
	if !prev.ok {
		return 0
	}

	prevIndex := make(map[string]string, len(prev.files))
	for _, f := range prev.files {
		prevIndex[f.Path] = f.Blob
	}

	var changed int64
	for _, f := range currentFiles {
		if prevBlob, ok := prevIndex[f.Path]; !ok || prevBlob != f.Blob {
			changed++
		}
		delete(prevIndex, f.Path)
	}
	// Whatever remains in prevIndex are deleted files.
	changed += int64(len(prevIndex))

	return changed
}

// spawnAutoPlaybook launches `semantica _auto-playbook` as a detached process.
func spawnAutoPlaybook(semDir, checkpointID, commitHash, repoRoot string) {
	exe, err := os.Executable()
	if err != nil {
		exe = "semantica"
	}

	logFile, err := util.OpenWorkerLog(semDir)
	if err != nil {
		wlog("worker: auto-playbook: open log failed: %v\n", err)
		return
	}

	cmd := exec.Command(exe, "_auto-playbook",
		"--checkpoint", checkpointID,
		"--commit", commitHash,
		"--repo", repoRoot,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		wlog("worker: auto-playbook: spawn failed: %v\n", err)
		_ = logFile.Close()
		return
	}

	_ = logFile.Close()
	wlog("worker: auto-playbook spawned for checkpoint %s\n", checkpointID)
}

// remotePushPayload is the JSON body POSTed to the remote attribution endpoint.
// NOTE: remote_url comes from `git remote get-url origin` which may be SSH or
// HTTPS. The backend must canonicalize this (e.g., normalize SSH and HTTPS,
// strip/add .git suffix) before using it as a repository identity key.
// providerDetail is the per-provider breakdown in the push payload.
type providerDetail struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	AILines  int    `json:"ai_lines"`
}

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
	CLIVersion       string                 `json:"cli_version,omitempty"`
	AttrVersion      string                 `json:"attribution_version"`
	PushedAt         int64                  `json:"pushed_at"`
}

// buildPushPayload constructs the remote attribution payload by combining
// the attribution result with git metadata and checkpoint-scoped enrichment
// from the local DB.
func buildPushPayload(ctx context.Context, h *sqlstore.Handle, result *AttributionResult, remoteURL, branch, commitHash, subject, checkpointID string) remotePushPayload {
	// Enrich with session metadata from local DB, scoped to this checkpoint.
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

	// Include playbook summary if already available (e.g., on re-push after
	// auto-playbook completes).
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
		CLIVersion:       version.Version,
		AttrVersion:      "v1",
		PushedAt:         time.Now().UnixMilli(),
	}
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

// pushAttribution is the log-only wrapper used by the existing worker call site.
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

// syncProvenance prepares and uploads packaged provenance manifests.
// Pass watermarkTs=0 to drain all packaged manifests.
func syncProvenance(ctx context.Context, repoRoot string, watermarkTs int64) {
	endpoint := auth.EffectiveEndpoint()
	token, tokenErr := auth.AccessToken(ctx)
	if tokenErr != nil {
		wlog("worker: sync-provenance: auth failed: %v\n", tokenErr)
		return
	}

	results, err := provenance.SyncAndUpload(ctx, repoRoot, endpoint, token, watermarkTs, 50, nil)
	if err != nil {
		wlog("worker: sync-provenance: %v\n", err)
		return
	}

	// On 401 for any result, refresh token and retry the full batch once.
	hasUnauth := false
	for _, r := range results {
		if r.Err != nil && provenance.IsUnauthorized(r.Err) {
			hasUnauth = true
			break
		}
	}
	if hasUnauth && token != "" && !auth.IsAPIKeyAuth() {
		refreshed, refreshErr := auth.ForceRefresh(ctx)
		if refreshErr != nil {
			wlog("worker: sync-provenance: refresh after 401 failed: %v\n", refreshErr)
			return
		}
		retryResults, retryErr := provenance.SyncAndUpload(ctx, repoRoot, endpoint, refreshed, watermarkTs, 50, nil)
		if retryErr != nil {
			wlog("worker: sync-provenance: retry after refresh: %v\n", retryErr)
			return
		}
		results = retryResults
	}

	for _, r := range results {
		if r.Err != nil {
			wlog("worker: sync-provenance: turn %s upload failed: %v\n", util.ShortID(r.TurnID), r.Err)
		} else if r.Uploaded {
			wlog("worker: sync-provenance: turn %s uploaded\n", util.ShortID(r.TurnID))
		}
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

// reconcileImplementations processes pending implementation observations.
// Best-effort: errors are logged, not propagated.
// Creates implementations.db on first call.
// Commit attachment is handled separately by attachCommitToImplementation
// after session_checkpoints have been written.
func reconcileImplementations(ctx context.Context, repoRoot string) {
	base, err := broker.GlobalBase()
	if err != nil {
		return
	}
	implPath := filepath.Join(base, "implementations.db")

	implH, err := impldb.Open(ctx, implPath, impldb.OpenOptions{
		BusyTimeout: 5 * time.Second,
		TxImmediate: true,
	})
	if err != nil {
		wlog("worker: open implementations db: %v\n", err)
		return
	}
	defer func() { _ = impldb.Close(implH) }()

	r := &implementations.Reconciler{}
	if _, err := r.Reconcile(ctx, implH); err != nil {
		wlog("worker: reconcile implementations: %v\n", err)
	}
}

// attachCommitToImplementation links a commit to its implementation.
// Must run after session_checkpoints have been written, because AttachCommit
// depends on commit_links + session_checkpoints to find which sessions
// belong to the commit's checkpoint.
func handleImplementationPostCommit(ctx context.Context, semDir, repoRoot, commitHash string) {
	base, err := broker.GlobalBase()
	if err != nil {
		wlog("worker: resolve implementations base: %v\n", err)
		return
	}
	implPath := filepath.Join(base, "implementations.db")

	implH, err := impldb.Open(ctx, implPath, impldb.DefaultOpenOptions())
	if err != nil {
		wlog("worker: open implementations db for commit attach: %v\n", err)
		return
	}
	defer func() { _ = impldb.Close(implH) }()

	r := &implementations.Reconciler{}
	if err := r.AttachCommit(ctx, implH, implementations.AttachCommitInput{
		RepoPath:   repoRoot,
		CommitHash: commitHash,
	}); err != nil {
		wlog("worker: attach commit to implementation: %v\n", err)
		return
	}

	if !util.IsImplementationSummaryEnabled(semDir) {
		return
	}

	// Resolve the implementation for this commit before running skip logic.
	canonicalPath := broker.CanonicalRepoPath(repoRoot)
	implID, err := implH.Queries.FindImplementationByCommit(ctx, impldbgen.FindImplementationByCommitParams{
		CanonicalPath: canonicalPath,
		CommitHash:    commitHash,
	})
	if err != nil {
		return // commit not attached to any implementation
	}

	// Run skip logic before writing the in-progress marker.
	if ok, reason := implementations.ShouldAutoSummarize(ctx, implH, implID, implementations.ShouldAutoSummarizeOpts{}); !ok {
		wlog("worker: auto-impl-summary: skip %s: %s\n", implID[:8], reason)
		return
	}

	// Mark the implementation before spawning the background process.
	if err := implementations.MarkGenerationInProgress(ctx, implH, implID); err != nil {
		wlog("worker: auto-impl-summary: mark in-progress: %v\n", err)
		return
	}

	if !spawnAutoImplementationSummary(semDir, implID) {
		implementations.ClearGenerationInProgress(ctx, implH, implID)
	}
}

// spawnAutoImplementationSummary launches `semantica _auto-implementation-summary`
// as a detached process. Returns true on success, false on failure.
func spawnAutoImplementationSummary(semDir, implID string) bool {
	exe, err := os.Executable()
	if err != nil {
		exe = "semantica"
	}

	logFile, err := util.OpenWorkerLog(semDir)
	if err != nil {
		wlog("worker: auto-impl-summary: open log failed: %v\n", err)
		return false
	}

	cmd := exec.Command(exe, "_auto-implementation-summary",
		"--impl", implID,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		wlog("worker: auto-impl-summary: spawn failed: %v\n", err)
		_ = logFile.Close()
		return false
	}

	_ = logFile.Close()
	wlog("worker: auto-impl-summary spawned for %s\n", implID[:8])
	return true
}

func failCheckpoint(ctx context.Context, h *sqlstore.Handle, checkpointID string) {
	if err := h.Queries.FailCheckpoint(ctx, sqldb.FailCheckpointParams{
		CompletedAt:  sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true},
		CheckpointID: checkpointID,
	}); err != nil {
		wlog("worker: fail checkpoint %s: %v\n", checkpointID, err)
	}
}

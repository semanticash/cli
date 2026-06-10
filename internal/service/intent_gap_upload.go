package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/semanticash/cli/internal/auth"
	"github.com/semanticash/cli/internal/git"
	"github.com/semanticash/cli/internal/intentgap"
	"github.com/semanticash/cli/internal/llm"
	"github.com/semanticash/cli/internal/providers"
	"github.com/semanticash/cli/internal/util"
)

// IntentGapUploadStatus is the high-level outcome of a single upload
// attempt.
type IntentGapUploadStatus string

const (
	UploadStatusUploaded  IntentGapUploadStatus = "uploaded"
	UploadStatusDuplicate IntentGapUploadStatus = "duplicate"
	UploadStatusSkipped   IntentGapUploadStatus = "skipped"
	UploadStatusError     IntentGapUploadStatus = "error"
)

// IntentGapUploadResult records the upload outcome for the caller.
type IntentGapUploadResult struct {
	Status     IntentGapUploadStatus
	Reason     string
	PRNumber   int32
	HeadSHA    string
	UploadID   string
	ReceivedAt string
	Provider   string
	Model      string
}

// IntentGapUploadDeps lets tests inject fake collaborators in place of
// the production wiring. A nil field falls back to the production
// helper, so callers in normal code can pass an empty struct.
type IntentGapUploadDeps struct {
	HTTPClient *http.Client
	// Endpoint defaults to auth.EffectiveEndpoint(); tests pin a
	// stub server URL here.
	Endpoint string
	// Token defaults to auth.AccessToken(ctx); tests pin a fixed value.
	Token string
	// Now defaults to time.Now; tests pin a fixed timestamp so the
	// produced_at field is deterministic.
	Now func() time.Time
	// LLMRegistry defaults to the production fallback chain; tests
	// can hand in a stub registry whose Find() resolves to whatever
	// shape they want to exercise.
	LLMRegistry *llm.WriterRegistry
	// DeviceID defaults to intentgap.LoadOrCreateDeviceID(); tests
	// pin a fixed value to avoid touching the user-global config dir.
	DeviceID string
}

// IntentGapUploadService runs the transport-only upload path used by
// the pre-push hook and user-facing intent-gap commands.
type IntentGapUploadService struct {
	deps IntentGapUploadDeps
}

func NewIntentGapUploadService(deps IntentGapUploadDeps) *IntentGapUploadService {
	return &IntentGapUploadService{deps: deps}
}

// Run executes the upload path against the repo at repoPath. The
// returned result distinguishes uploaded / duplicate / skipped /
// error so the caller can render the outcome uniformly. An error
// return covers infrastructure failures (DB / HTTP / git); skip
// outcomes are reported via Status, not via the error.
//
// Code paths that reach .semantica write their outcome to the
// activity log so background workers leave a doctor-visible trail.
// Errors before repo state is available surface only via the returned
// error.
func (s *IntentGapUploadService) Run(ctx context.Context, repoPath string) (*IntentGapUploadResult, error) {
	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return nil, fmt.Errorf("open repo: %w", err)
	}
	repoRoot := repo.Root()
	semDir := filepath.Join(repoRoot, ".semantica")

	if !util.IsEnabled(semDir) {
		return skipped(semDir, "semantica not enabled"), nil
	}
	if !util.IntentGapEnabled(semDir) {
		return skipped(semDir, "intent_gap.enabled is false"), nil
	}

	settings, err := util.ReadSettings(semDir)
	if err != nil {
		util.AppendActivityLog(semDir, "intent-gap error: read settings: %v", err)
		return nil, fmt.Errorf("read settings: %w", err)
	}
	if !settings.Connected || settings.ConnectedRepoID == "" {
		return skipped(semDir, "repo not connected"), nil
	}

	branch, err := repo.CurrentBranch(ctx)
	if err != nil || branch == "" || branch == "HEAD" {
		return skipped(semDir, "current branch unavailable"), nil
	}
	headSHA, err := repo.HeadCommitHash(ctx)
	if err != nil || headSHA == "" {
		return skipped(semDir, "HEAD SHA unavailable"), nil
	}

	endpoint := s.deps.Endpoint
	if endpoint == "" {
		endpoint = auth.EffectiveEndpoint()
	}
	token := s.deps.Token
	if token == "" {
		token, err = auth.AccessToken(ctx)
		if err != nil || token == "" {
			return skipped(semDir, "CLI not authenticated"), nil
		}
	}

	pr, err := intentgap.LookupOpenPRByBranch(ctx, s.deps.HTTPClient, endpoint, token, settings.ConnectedRepoID, branch)
	switch {
	case errors.Is(err, intentgap.ErrNoOpenPR):
		return skipped(semDir, fmt.Sprintf("no open PR for branch %q", branch)), nil
	case errors.Is(err, intentgap.ErrAmbiguousPR):
		var ambig *intentgap.AmbiguousPRError
		if errors.As(err, &ambig) {
			return skipped(semDir, fmt.Sprintf("%d open PRs match branch %q; resolve before analyzing", len(ambig.Matches), branch)), nil
		}
		return skipped(semDir, fmt.Sprintf("multiple open PRs match branch %q", branch)), nil
	case errors.Is(err, intentgap.ErrUnavailable):
		return skipped(semDir, "PR-context discovery unavailable: "+err.Error()), nil
	case err != nil:
		util.AppendActivityLog(semDir, "intent-gap error: PR lookup: %v", err)
		return nil, fmt.Errorf("PR lookup: %w", err)
	}

	registry := s.deps.LLMRegistry
	if registry == nil {
		registry = providers.NewWriterRegistry()
	}
	provider, err := intentgap.PickInstalledProvider(registry)
	if err != nil {
		return skipped(semDir, "no LLM CLI installed; nothing to record"), nil
	}

	deviceID := s.deps.DeviceID
	if deviceID == "" {
		deviceID, err = intentgap.LoadOrCreateDeviceID()
		if err != nil {
			util.AppendActivityLog(semDir, "intent-gap error: device id: %v", err)
			return nil, fmt.Errorf("device id: %w", err)
		}
	}

	now := time.Now
	if s.deps.Now != nil {
		now = s.deps.Now
	}

	in := intentgap.UploadInput{
		RepositoryID:     settings.ConnectedRepoID,
		PRNumber:         pr.PRNumber,
		HeadSHA:          headSHA,
		BaseSHA:          "",
		Provider:         provider.Name,
		Model:            provider.Model,
		ProducerDeviceID: deviceID,
	}
	body, hash, err := intentgap.BuildTransportOnlyBody(in, now())
	if err != nil {
		util.AppendActivityLog(semDir, "intent-gap error: build body: %v", err)
		return nil, fmt.Errorf("build body: %w", err)
	}

	uploadRes, err := intentgap.PostUpload(ctx, s.deps.HTTPClient, endpoint, token, in, body, hash)
	if err != nil {
		util.AppendActivityLog(semDir, "intent-gap upload error PR #%d: %v", pr.PRNumber, err)
		return &IntentGapUploadResult{
			Status:   UploadStatusError,
			Reason:   err.Error(),
			PRNumber: pr.PRNumber,
			HeadSHA:  headSHA,
			Provider: provider.Name,
			Model:    provider.Model,
		}, nil
	}

	status := UploadStatusUploaded
	if uploadRes.StatusCode == http.StatusOK {
		status = UploadStatusDuplicate
	}
	util.AppendActivityLog(semDir, "intent-gap %s PR #%d upload_id=%s", status, pr.PRNumber, uploadRes.UploadID)
	return &IntentGapUploadResult{
		Status:     status,
		PRNumber:   pr.PRNumber,
		HeadSHA:    headSHA,
		UploadID:   uploadRes.UploadID,
		ReceivedAt: uploadRes.ReceivedAt,
		Provider:   provider.Name,
		Model:      provider.Model,
	}, nil
}

// skipped builds a skip result and records the reason when repo-local
// Semantica state already exists.
//
// AppendActivityLog creates its target directory, so this helper stats
// semDir first to avoid creating .semantica in a repo that never opted
// in.
func skipped(semDir, reason string) *IntentGapUploadResult {
	if semDir != "" {
		if info, err := os.Stat(semDir); err == nil && info.IsDir() {
			util.AppendActivityLog(semDir, "intent-gap skipped: %s", reason)
		}
	}
	return &IntentGapUploadResult{Status: UploadStatusSkipped, Reason: reason}
}

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

// IntentGapUploadStatus is the high-level outcome of an upload attempt.
type IntentGapUploadStatus string

const (
	UploadStatusUploaded  IntentGapUploadStatus = "uploaded"
	UploadStatusDuplicate IntentGapUploadStatus = "duplicate"
	UploadStatusSkipped   IntentGapUploadStatus = "skipped"
	UploadStatusError     IntentGapUploadStatus = "error"
)

// IntentGapUploadResult records transport and analysis outcomes separately.
type IntentGapUploadResult struct {
	Status     IntentGapUploadStatus
	Reason     string
	PRNumber   int32
	HeadSHA    string
	UploadID   string
	ReceivedAt string
	Provider   string
	Model      string
	// Analysis is empty when execution stops before analysis.
	Analysis AnalysisOutcome
	// AnalysisReason contains a sanitized code for errored analysis.
	AnalysisReason string
}

// AnalysisOutcome reports whether local analysis completed or errored.
type AnalysisOutcome string

const (
	AnalysisAnalyzed AnalysisOutcome = "analyzed"
	AnalysisErrored  AnalysisOutcome = "errored"
)

// IntentGapUploadDeps provides optional collaborators for the upload service.
type IntentGapUploadDeps struct {
	HTTPClient *http.Client
	// BaseRef overrides automatic base-branch detection for manual analysis.
	BaseRef string
	// Endpoint defaults to auth.EffectiveEndpoint.
	Endpoint string
	// Token defaults to the current CLI access token.
	Token string
	// Now defaults to time.Now.
	Now func() time.Time
	// LLMRegistry defaults to the configured local AI fallback chain.
	LLMRegistry *llm.WriterRegistry
	// DeviceID defaults to the persisted installation identifier.
	DeviceID string
	// BundleAssembler defaults to the Git and lineage-backed assembler.
	BundleAssembler intentgap.BundleAssembler
	// Analyzer defaults to the local LLM analyzer.
	Analyzer intentgap.IntentGapAnalyzer
}

// IntentGapUploadService analyzes the current pull request and records the result.
type IntentGapUploadService struct {
	deps IntentGapUploadDeps
}

func NewIntentGapUploadService(deps IntentGapUploadDeps) *IntentGapUploadService {
	return &IntentGapUploadService{deps: deps}
}

// Run analyzes the current PR and records the result. Expected skip outcomes
// are returned in Status; infrastructure failures are returned as errors.
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

	analysis, err := s.runAnalysisAndBuildBody(ctx, semDir, in, pr, repoRoot, now)
	if err != nil {
		util.AppendActivityLog(semDir, "intent-gap error: build body: %v", err)
		return nil, fmt.Errorf("build body: %w", err)
	}
	// Prefer the writer that produced the analysis when one was invoked.
	if analysis.Provider != "" {
		in.Provider = analysis.Provider
	}
	if analysis.Model != "" {
		in.Model = analysis.Model
	}
	in.BaseSHA = analysis.BaseSHA

	uploadRes, err := intentgap.PostUpload(ctx, s.deps.HTTPClient, endpoint, token, in, analysis.Body, analysis.Hash)
	if err != nil {
		util.AppendActivityLog(semDir, "intent-gap upload error PR #%d: %v", pr.PRNumber, err)
		return &IntentGapUploadResult{
			Status:         UploadStatusError,
			Reason:         err.Error(),
			PRNumber:       pr.PRNumber,
			HeadSHA:        headSHA,
			Provider:       in.Provider,
			Model:          in.Model,
			Analysis:       analysis.Outcome,
			AnalysisReason: analysis.Reason,
		}, nil
	}

	status := UploadStatusUploaded
	if uploadRes.StatusCode == http.StatusOK {
		status = UploadStatusDuplicate
	}
	// Keep the final activity line aligned with the analysis outcome.
	switch analysis.Outcome {
	case AnalysisErrored:
		util.AppendActivityLog(semDir, "intent-gap analysis errored reason=%s PR #%d upload=%s upload_id=%s",
			analysis.Reason, pr.PRNumber, status, uploadRes.UploadID)
	default:
		util.AppendActivityLog(semDir, "intent-gap %s PR #%d upload_id=%s", status, pr.PRNumber, uploadRes.UploadID)
	}
	return &IntentGapUploadResult{
		Status:         status,
		PRNumber:       pr.PRNumber,
		HeadSHA:        headSHA,
		UploadID:       uploadRes.UploadID,
		ReceivedAt:     uploadRes.ReceivedAt,
		Provider:       in.Provider,
		Model:          in.Model,
		Analysis:       analysis.Outcome,
		AnalysisReason: analysis.Reason,
	}, nil
}

// analysisProduct contains a prepared upload and its analysis metadata.
type analysisProduct struct {
	Body     []byte
	Hash     string
	Outcome  AnalysisOutcome
	Reason   string
	Provider string
	Model    string
	BaseSHA  string
}

// runAnalysisAndBuildBody prepares either an analyzed or errored upload.
func (s *IntentGapUploadService) runAnalysisAndBuildBody(
	ctx context.Context,
	semDir string,
	in intentgap.UploadInput,
	pr *intentgap.OpenPR,
	repoRoot string,
	now func() time.Time,
) (analysisProduct, error) {
	assembler := s.deps.BundleAssembler
	if assembler == nil {
		assembler = intentgap.NewGitBundleAssembler(defaultGitOpener, newSQLiteTurnLoader(repoRoot))
	}
	analyzer := s.deps.Analyzer
	if analyzer == nil {
		// Use the same fallback chain used for provider selection.
		registry := s.deps.LLMRegistry
		if registry == nil {
			registry = providers.NewWriterRegistry()
		}
		analyzer = intentgap.NewLLMAnalyzer(registry)
	}

	bundle, bundleErr := assembler.Assemble(ctx, intentgap.BundleInput{
		RepoRoot: repoRoot,
		Base:     s.deps.BaseRef,
		HeadSHA:  in.HeadSHA,
	})
	if bundleErr != nil {
		// Keep local error details out of the uploaded reason code.
		util.AppendActivityLog(semDir, "intent-gap bundle assembly failed PR #%d: %v", pr.PRNumber, bundleErr)
		// Preserve actionable failure categories without uploading local details.
		reason := string(intentgap.ReasonBundleFailed)
		switch {
		case errors.Is(bundleErr, intentgap.ErrLineageUnavailable):
			reason = string(intentgap.ReasonLineageUnavailable)
		case errors.Is(bundleErr, intentgap.ErrRedactionFailed):
			reason = string(intentgap.ReasonRedactionFailed)
		}
		body, hash, err := intentgap.BuildErroredBody(in, reason, intentgap.PromptTemplateVersion, now())
		return analysisProduct{Body: body, Hash: hash, Outcome: AnalysisErrored, Reason: reason}, err
	}

	result, analyzerErr := analyzer.Analyze(ctx, intentgap.AnalysisInput{
		Bundle:       bundle,
		PRNumber:     pr.PRNumber,
		RepositoryID: in.RepositoryID,
	})
	if analyzerErr != nil {
		// Keep subprocess details local and upload only a sanitized code.
		util.AppendActivityLog(semDir, "intent-gap analyzer failed PR #%d: %v", pr.PRNumber, analyzerErr)
		reason := string(intentgap.ReasonCodeFor(analyzerErr))
		// Preserve the resolved base SHA in errored results.
		erroredIn := in
		erroredIn.BaseSHA = bundle.BaseSHA
		body, hash, err := intentgap.BuildErroredBody(erroredIn, reason, intentgap.PromptTemplateVersion, now())
		return analysisProduct{Body: body, Hash: hash, Outcome: AnalysisErrored, Reason: reason, BaseSHA: bundle.BaseSHA}, err
	}

	body, hash, err := intentgap.BuildAnalyzedBody(intentgap.AnalyzedBodyInput{
		UploadInput:           withProviderModel(in, result.Provider, result.Model, bundle.BaseSHA),
		PromptTemplateVersion: result.PromptTemplateVersion,
		Findings:              result.Findings,
		CoverageSummary:       result.CoverageSummary,
	}, now())
	return analysisProduct{
		Body:     body,
		Hash:     hash,
		Outcome:  AnalysisAnalyzed,
		Provider: result.Provider,
		Model:    result.Model,
		BaseSHA:  bundle.BaseSHA,
	}, err
}

// withProviderModel applies analyzer attribution without clearing preselected
// values when analysis does not invoke a writer.
func withProviderModel(in intentgap.UploadInput, provider, model, baseSHA string) intentgap.UploadInput {
	if provider != "" {
		in.Provider = provider
	}
	if model != "" {
		in.Model = model
	}
	in.BaseSHA = baseSHA
	return in
}

// defaultGitOpener adapts git.OpenRepo to the bundle assembler interface.
func defaultGitOpener(repoPath string) (intentgap.GitRepo, error) {
	r, err := git.OpenRepo(repoPath)
	if err != nil {
		return nil, err
	}
	return gitRepoAdapter{r: r}, nil
}

// gitRepoAdapter implements intentgap.GitRepo over git.Repo.
type gitRepoAdapter struct{ r *git.Repo }

func (a gitRepoAdapter) DefaultBaseRef(ctx context.Context) (string, error) {
	return a.r.DefaultBaseRef(ctx)
}
func (a gitRepoAdapter) MergeBase(ctx context.Context, x, y string) (string, error) {
	return a.r.MergeBase(ctx, x, y)
}
func (a gitRepoAdapter) DiffBetween(ctx context.Context, x, y string) ([]byte, error) {
	return a.r.DiffBetween(ctx, x, y)
}
func (a gitRepoAdapter) CountCommitsBetween(ctx context.Context, x, y string) (int, error) {
	return a.r.CountCommitsBetween(ctx, x, y)
}
func (a gitRepoAdapter) CommitSummariesBetween(ctx context.Context, x, y string, limit int) ([]intentgap.CommitMetaBetween, error) {
	rows, err := a.r.CommitSummariesBetween(ctx, x, y, limit)
	if err != nil {
		return nil, err
	}
	out := make([]intentgap.CommitMetaBetween, len(rows))
	for i, r := range rows {
		out[i] = intentgap.CommitMetaBetween{Hash: r.Hash, Subject: r.Subject}
	}
	return out, nil
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

package service

import (
	"context"
	"encoding/json"
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
	// UploadStatusAnalyzed marks a local run that produced findings
	// without uploading them.
	UploadStatusAnalyzed IntentGapUploadStatus = "analyzed"
)

// RunOptions controls one call to Run.
type RunOptions struct {
	// Upload toggles the API record step. When false, the analyzer
	// still runs (or a cached result is reused) and findings come back
	// in the result for local rendering, but nothing is uploaded.
	Upload bool
}

// IntentGapUploadResult records transport and analysis outcomes separately.
//
// Findings and CoverageSummary are populated whenever the analyzer
// produced a result (fresh run or cache hit) so the command layer can
// render them locally; they are also what the API received when
// Upload is true.
type IntentGapUploadResult struct {
	Status     IntentGapUploadStatus
	Reason     string
	PRNumber   int32
	HeadSHA    string
	BaseSHA    string
	UploadID   string
	ReceivedAt string
	Provider   string
	Model      string
	// Analysis is empty when execution stops before analysis.
	Analysis AnalysisOutcome
	// AnalysisReason contains a sanitized code for errored analysis.
	AnalysisReason string
	// Findings and CoverageSummary are the analyzer output, populated
	// for both fresh runs and cache hits. Nil only when execution
	// stopped before analysis (skip / pre-analysis error).
	Findings              json.RawMessage
	CoverageSummary       json.RawMessage
	PromptTemplateVersion string
	// UsedCache is true when the analyzer step was skipped because a
	// matching cache entry was found for the current head_sha.
	UsedCache bool
}

// AnalysisOutcome reports whether local analysis completed or errored.
type AnalysisOutcome string

const (
	AnalysisAnalyzed AnalysisOutcome = "analyzed"
	AnalysisErrored  AnalysisOutcome = "errored"
)

// errNoLLMInstalled marks a cache miss that cannot run fresh analysis
// because no local LLM CLI is available.
var errNoLLMInstalled = errors.New("intent-gap: no LLM CLI installed for fresh analysis")

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

// Run analyzes the current PR and optionally records the result. With
// opts.Upload=false the analyzer (or a cache hit) produces findings and
// the function returns; with opts.Upload=true the same findings are
// posted to the API. Expected skip outcomes come back in Status;
// infrastructure failures are returned as errors.
func (s *IntentGapUploadService) Run(ctx context.Context, repoPath string, opts RunOptions) (*IntentGapUploadResult, error) {
	repo, err := git.OpenRepo(repoPath)
	if err != nil {
		return nil, fmt.Errorf("open repo: %w", err)
	}
	repoRoot := repo.Root()
	semDir := filepath.Join(repoRoot, ".semantica")

	if !util.IsEnabled(semDir) {
		return skipped(semDir, "semantica not enabled"), nil
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

	// Provider/Model are populated by obtainAnalysis once it knows whether
	// the run is a cache hit (use cached values) or a fresh analysis
	// (use the picked provider). Leaving them empty here defers the
	// "must have an LLM CLI" check until cache lookup misses.
	in := intentgap.UploadInput{
		RepositoryID:     settings.ConnectedRepoID,
		PRNumber:         pr.PRNumber,
		HeadSHA:          headSHA,
		BaseSHA:          "",
		ProducerDeviceID: deviceID,
	}

	// If no cache file exists, fresh analysis is required, so resolve
	// the provider before bundle assembly. If a cache file exists,
	// defer provider lookup until after the freshness check.
	if !intentgap.CacheFileExists(semDir, headSHA) {
		registry := s.deps.LLMRegistry
		if registry == nil {
			registry = providers.NewWriterRegistry()
		}
		provider, providerErr := intentgap.PickInstalledProvider(registry)
		if providerErr != nil {
			return skipped(semDir, "no LLM CLI installed; nothing to record"), nil
		}
		in.Provider = provider.Name
		in.Model = provider.Model
	}

	analysis, usedCache, err := s.obtainAnalysis(ctx, semDir, in, pr, repoRoot, now)
	if errors.Is(err, errNoLLMInstalled) {
		return skipped(semDir, "no LLM CLI installed; nothing to record"), nil
	}
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

	// Local mode returns findings to the caller without uploading them.
	// Errored analyses surface through AnalysisReason.
	if !opts.Upload {
		return &IntentGapUploadResult{
			Status:                UploadStatusAnalyzed,
			PRNumber:              pr.PRNumber,
			HeadSHA:               headSHA,
			BaseSHA:               in.BaseSHA,
			Provider:              in.Provider,
			Model:                 in.Model,
			Analysis:              analysis.Outcome,
			AnalysisReason:        analysis.Reason,
			Findings:              analysis.Findings,
			CoverageSummary:       analysis.CoverageSummary,
			PromptTemplateVersion: analysis.PromptTemplateVersion,
			UsedCache:             usedCache,
		}, nil
	}

	uploadRes, err := intentgap.PostUpload(ctx, s.deps.HTTPClient, endpoint, token, in, analysis.Body, analysis.Hash)
	if err != nil {
		util.AppendActivityLog(semDir, "intent-gap upload error PR #%d: %v", pr.PRNumber, err)
		return &IntentGapUploadResult{
			Status:                UploadStatusError,
			Reason:                err.Error(),
			PRNumber:              pr.PRNumber,
			HeadSHA:               headSHA,
			BaseSHA:               in.BaseSHA,
			Provider:              in.Provider,
			Model:                 in.Model,
			Analysis:              analysis.Outcome,
			AnalysisReason:        analysis.Reason,
			Findings:              analysis.Findings,
			CoverageSummary:       analysis.CoverageSummary,
			PromptTemplateVersion: analysis.PromptTemplateVersion,
			UsedCache:             usedCache,
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
		Status:                status,
		PRNumber:              pr.PRNumber,
		HeadSHA:               headSHA,
		BaseSHA:               in.BaseSHA,
		UploadID:              uploadRes.UploadID,
		ReceivedAt:            uploadRes.ReceivedAt,
		Provider:              in.Provider,
		Model:                 in.Model,
		Analysis:              analysis.Outcome,
		AnalysisReason:        analysis.Reason,
		Findings:              analysis.Findings,
		CoverageSummary:       analysis.CoverageSummary,
		PromptTemplateVersion: analysis.PromptTemplateVersion,
		UsedCache:             usedCache,
	}, nil
}

// analysisProduct contains a prepared upload and its analysis metadata.
//
// Findings and CoverageSummary mirror what was uploaded; they let the
// command layer render results without re-parsing the wire body, and
// they feed the on-disk cache so a subsequent --upload can replay the
// same analysis without invoking the LLM again.
type analysisProduct struct {
	Body                  []byte
	Hash                  string
	Outcome               AnalysisOutcome
	Reason                string
	Provider              string
	Model                 string
	BaseSHA               string
	Findings              json.RawMessage
	CoverageSummary       json.RawMessage
	PromptTemplateVersion string
	AnalyzedAt            time.Time
}

// assembleBundle runs the bundle assembler and returns the bundle on
// success. On failure it returns an errored analysisProduct ready for
// upload (hadErrored=true) without attempting analysis or cache
// lookup. A non-nil error means infrastructure-level failure that the
// caller should surface up the stack.
func (s *IntentGapUploadService) assembleBundle(
	ctx context.Context,
	semDir string,
	in intentgap.UploadInput,
	pr *intentgap.OpenPR,
	repoRoot string,
	now func() time.Time,
) (intentgap.Bundle, analysisProduct, bool, error) {
	assembler := s.deps.BundleAssembler
	if assembler == nil {
		assembler = intentgap.NewGitBundleAssembler(
			defaultGitOpener,
			newSQLiteTurnLoader(repoRoot),
			newSQLiteAgentActionLoader(repoRoot),
		)
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
		// Preserve Provider/Model so errored results name the
		// selected writer.
		return intentgap.Bundle{}, analysisProduct{
			Body:     body,
			Hash:     hash,
			Outcome:  AnalysisErrored,
			Reason:   reason,
			Provider: in.Provider,
			Model:    in.Model,
		}, true, err
	}
	return bundle, analysisProduct{}, false, nil
}

// runAnalyzerOnBundle invokes the analyzer against a pre-assembled
// bundle and packages the result for upload or local rendering. On
// analyzer failure it returns an errored product whose BaseSHA is
// still populated so the upload can surface the resolved base.
func (s *IntentGapUploadService) runAnalyzerOnBundle(
	ctx context.Context,
	semDir string,
	in intentgap.UploadInput,
	pr *intentgap.OpenPR,
	bundle intentgap.Bundle,
	now func() time.Time,
) (analysisProduct, error) {
	analyzer := s.deps.Analyzer
	if analyzer == nil {
		registry := s.deps.LLMRegistry
		if registry == nil {
			registry = providers.NewWriterRegistry()
		}
		analyzer = intentgap.NewLLMAnalyzer(registry)
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
		// Preserve Provider/Model so errored results name the
		// selected writer.
		return analysisProduct{
			Body:     body,
			Hash:     hash,
			Outcome:  AnalysisErrored,
			Reason:   reason,
			Provider: in.Provider,
			Model:    in.Model,
			BaseSHA:  bundle.BaseSHA,
		}, err
	}

	// Surface structural schema-or-drop diagnostics locally without
	// uploading raw finding bodies.
	for _, sample := range result.SchemaDiagnostics {
		util.AppendActivityLog(semDir, "intent-gap schema-dropped finding PR #%d: %s", pr.PRNumber, sample)
	}

	analyzedAt := now()
	body, hash, err := intentgap.BuildAnalyzedBody(intentgap.AnalyzedBodyInput{
		UploadInput:           withProviderModel(in, result.Provider, result.Model, bundle.BaseSHA),
		PromptTemplateVersion: result.PromptTemplateVersion,
		Findings:              result.Findings,
		CoverageSummary:       result.CoverageSummary,
	}, analyzedAt)
	return analysisProduct{
		Body:                  body,
		Hash:                  hash,
		Outcome:               AnalysisAnalyzed,
		Provider:              result.Provider,
		Model:                 result.Model,
		BaseSHA:               bundle.BaseSHA,
		Findings:              result.Findings,
		CoverageSummary:       result.CoverageSummary,
		PromptTemplateVersion: result.PromptTemplateVersion,
		AnalyzedAt:            analyzedAt,
	}, err
}

// obtainAnalysis returns an analysisProduct ready to upload or render.
//
// Bundle assembly runs before cache lookup so the key can include the
// resolved BaseSHA. A cache hit skips provider lookup and LLM analysis;
// a bundle error is returned directly instead of serving stale cached
// findings.
//
// The cache is never used for errored outcomes: an errored row carries
// no findings to replay, and re-running may succeed where the cached
// run failed.
func (s *IntentGapUploadService) obtainAnalysis(
	ctx context.Context,
	semDir string,
	in intentgap.UploadInput,
	pr *intentgap.OpenPR,
	repoRoot string,
	now func() time.Time,
) (analysisProduct, bool, error) {
	bundle, erroredProduct, hadErrored, err := s.assembleBundle(ctx, semDir, in, pr, repoRoot, now)
	if err != nil {
		return analysisProduct{}, false, err
	}
	if hadErrored {
		return erroredProduct, false, nil
	}

	cacheKey := intentgap.AnalysisCacheKey{
		HeadSHA:               in.HeadSHA,
		BaseSHA:               bundle.BaseSHA,
		PromptTemplateVersion: intentgap.PromptTemplateVersion,
		FindingSchemaVersion:  intentgap.FindingSchemaVersion,
		RepositoryID:          in.RepositoryID,
		PRNumber:              pr.PRNumber,
		RequestedBase:         s.deps.BaseRef,
	}
	if cached, hit, readErr := intentgap.ReadAnalysisCache(semDir, cacheKey); readErr == nil && hit {
		if product, ok := s.rebuildFromCache(cached, in, now); ok {
			return product, true, nil
		}
	} else if readErr != nil {
		util.AppendActivityLog(semDir, "intent-gap cache read failed for head=%s: %v", in.HeadSHA, readErr)
	}

	// Cache miss: ensure a provider is available before invoking the
	// analyzer.
	if in.Provider == "" {
		registry := s.deps.LLMRegistry
		if registry == nil {
			registry = providers.NewWriterRegistry()
		}
		provider, providerErr := intentgap.PickInstalledProvider(registry)
		if providerErr != nil {
			return analysisProduct{}, false, errNoLLMInstalled
		}
		in.Provider = provider.Name
		in.Model = provider.Model
	}

	product, err := s.runAnalyzerOnBundle(ctx, semDir, in, pr, bundle, now)
	if err != nil {
		return product, false, err
	}
	if product.Outcome == AnalysisAnalyzed {
		cacheEntry := &intentgap.AnalysisCache{
			SchemaVersion:         intentgap.AnalysisCacheSchemaVersion,
			FindingSchemaVersion:  intentgap.FindingSchemaVersion,
			HeadSHA:               in.HeadSHA,
			BaseSHA:               product.BaseSHA,
			RequestedBase:         s.deps.BaseRef,
			PRNumber:              pr.PRNumber,
			RepositoryID:          in.RepositoryID,
			PromptTemplateVersion: product.PromptTemplateVersion,
			Provider:              product.Provider,
			Model:                 product.Model,
			AnalyzedAt:            product.AnalyzedAt,
			Findings:              product.Findings,
			CoverageSummary:       product.CoverageSummary,
		}
		if cacheErr := intentgap.WriteAnalysisCache(semDir, cacheEntry); cacheErr != nil {
			util.AppendActivityLog(semDir, "intent-gap cache write failed for head=%s: %v", in.HeadSHA, cacheErr)
		}
	}
	return product, false, nil
}

// rebuildFromCache turns a cached analysis back into an analysisProduct
// suitable for upload or local rendering. Returns (_, false) when the
// cache entry cannot rebuild a canonical upload body; the caller then
// falls back to running the analyzer fresh.
func (s *IntentGapUploadService) rebuildFromCache(
	cached *intentgap.AnalysisCache,
	in intentgap.UploadInput,
	now func() time.Time,
) (analysisProduct, bool) {
	uploadIn := withProviderModel(in, cached.Provider, cached.Model, cached.BaseSHA)
	producedAt := cached.AnalyzedAt
	if producedAt.IsZero() {
		producedAt = now()
	}
	body, hash, err := intentgap.BuildAnalyzedBody(intentgap.AnalyzedBodyInput{
		UploadInput:           uploadIn,
		PromptTemplateVersion: cached.PromptTemplateVersion,
		Findings:              cached.Findings,
		CoverageSummary:       cached.CoverageSummary,
	}, producedAt)
	if err != nil {
		return analysisProduct{}, false
	}
	return analysisProduct{
		Body:                  body,
		Hash:                  hash,
		Outcome:               AnalysisAnalyzed,
		Provider:              cached.Provider,
		Model:                 cached.Model,
		BaseSHA:               cached.BaseSHA,
		Findings:              cached.Findings,
		CoverageSummary:       cached.CoverageSummary,
		PromptTemplateVersion: cached.PromptTemplateVersion,
		AnalyzedAt:            producedAt,
	}, true
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

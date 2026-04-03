package provenance

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/util"
)

// SyncResult holds the prepared upload artifacts for a single turn.
type SyncResult struct {
	TurnID         string
	ManifestID     string
	ObjectCount    int
	UploadAttempts int64             // Current attempt count from the manifest row.
	Envelope       []byte            // JSON manifest envelope for registration.
	RedactedBlobs  map[string][]byte // Upload-hash to redacted blob bytes.
	Skipped        bool              // True if a required blob was missing.
}

// syncObject represents a single uploaded object in the envelope.
// Step-level metadata (event_id, tool_name, provenance_hash, file_paths)
// lives in the bundle blob, not here.
type syncObject struct {
	Kind      string `json:"kind"`
	Hash      string `json:"hash"`
	SizeBytes int    `json:"size_bytes"`
}

// syncEnvelope is the manifest envelope used to register a prepared turn.
// step_count, file_count, and prompt_hash are derived from the bundle blob
// by the receiver, not sent by the CLI.
type syncEnvelope struct {
	ConnectedRepoID   string       `json:"connected_repo_id"`
	Provider          string       `json:"provider"`
	ProviderSessionID string       `json:"provider_session_id"`
	TurnID            string       `json:"turn_id"`
	Model             string       `json:"model,omitempty"`
	CommitHash        string       `json:"commit_hash,omitempty"`
	CheckpointID      string       `json:"checkpoint_id,omitempty"`
	GitAuthor         string       `json:"git_author,omitempty"`
	StartedAt         int64        `json:"started_at"`
	CompletedAt       int64        `json:"completed_at,omitempty"`
	Objects           []syncObject `json:"objects"`
}

// staleUploadingThreshold is how long a manifest can stay in 'uploading'
// before being reset to 'packaged' for retry.
const staleUploadingThreshold = 5 * time.Minute

// SyncPendingTurns prepares upload artifacts for packaged manifests.
// The local schema includes upload_transform_version and
// remote_verified_at on provenance_manifests, but the current upload path
// does not write them yet. Wire these when adding GC or version-aware
// re-upload logic.
// The watermarkTs bounds the sync to manifests created at or before this
// timestamp. Pass 0 to drain all packaged manifests.
func SyncPendingTurns(ctx context.Context, repoPath string, watermarkTs int64, limit int) ([]SyncResult, error) {
	semDir := filepath.Join(repoPath, ".semantica")
	dbPath := filepath.Join(semDir, "lineage.db")

	h, err := sqlstore.Open(ctx, dbPath, sqlstore.OpenOptions{
		BusyTimeout: 100 * time.Millisecond,
		Synchronous: "NORMAL",
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlstore.Close(h) }()

	// Read connected repo ID from settings.
	settings, err := util.ReadSettings(semDir)
	if err != nil || settings.ConnectedRepoID == "" {
		return nil, nil // not connected - nothing to sync
	}

	repo, err := h.Queries.GetRepositoryByRootPath(ctx, repoPath)
	if err != nil {
		return nil, err
	}

	bs, err := blobs.NewStore(filepath.Join(semDir, "objects"))
	if err != nil {
		return nil, err
	}

	// Recover manifests stuck in 'uploading' from a previous crash.
	now := time.Now().UnixMilli()
	threshold := now - staleUploadingThreshold.Milliseconds()
	if err := h.Queries.RecoverStaleUploading(ctx, sqldb.RecoverStaleUploadingParams{
		UpdatedAt:   now,
		UpdatedAt_2: threshold,
	}); err != nil {
		slog.Warn("sync: recover stale uploading failed", "repo", repoPath, "err", err)
	}

	manifests, err := h.Queries.ListPackagedManifests(ctx, sqldb.ListPackagedManifestsParams{
		RepositoryID: repo.RepositoryID,
		Column2:      watermarkTs,
		CreatedAt:    watermarkTs,
		Limit:        int64(limit),
	})
	if err != nil {
		return nil, err
	}

	var results []SyncResult
	for _, m := range manifests {
		result := buildSyncResult(ctx, h, bs, &settings, m, repoPath)
		results = append(results, result)
	}

	return results, nil
}

func buildSyncResult(
	ctx context.Context,
	h *sqlstore.Handle,
	bs *blobs.Store,
	settings *util.Settings,
	m sqldb.ProvenanceManifest,
	repoPath string,
) SyncResult {
	result := SyncResult{
		TurnID:         m.TurnID,
		ManifestID:     m.ManifestID,
		UploadAttempts: m.UploadAttempts,
		RedactedBlobs:  make(map[string][]byte),
	}

	// Load raw bundle first (needed to extract hash references).
	if !m.ProvenanceBundleHash.Valid || m.ProvenanceBundleHash.String == "" {
		markFailed(ctx, h, m.ManifestID, "no bundle hash on manifest")
		result.Skipped = true
		return result
	}
	rawBundle, err := bs.Get(ctx, m.ProvenanceBundleHash.String)
	if err != nil {
		markFailed(ctx, h, m.ManifestID, "missing bundle blob: "+err.Error())
		result.Skipped = true
		return result
	}

	// Hash remap table: local CAS hash -> upload hash.
	// Built from prompt and step provenance blobs, then applied to the
	// bundle before its own redaction so uploaded hashes are consistent.
	hashMap := make(map[string]string)
	var objects []syncObject

	// Prompt blob - required when bundle references one.
	promptLocalHash := extractPromptHashFromBytes(rawBundle)
	if promptLocalHash != "" {
		hash, redacted, err := loadAndRedact(ctx, bs, promptLocalHash, "prompt", repoPath)
		if err != nil {
			markFailed(ctx, h, m.ManifestID, "prompt blob referenced by bundle but missing locally: "+err.Error())
			result.Skipped = true
			return result
		}
		hashMap[promptLocalHash] = hash
		result.RedactedBlobs[hash] = redacted
		objects = append(objects, syncObject{Kind: "prompt", Hash: hash, SizeBytes: len(redacted)})
	}

	// Step provenance blobs - required when bundle references them.
	stepLocalHashes := extractStepProvenanceHashes(rawBundle)
	for _, localHash := range stepLocalHashes {
		hash, redacted, err := loadAndRedact(ctx, bs, localHash, "step_provenance", repoPath)
		if err != nil {
			markFailed(ctx, h, m.ManifestID, "step provenance blob "+localHash[:8]+" referenced by bundle but missing locally: "+err.Error())
			result.Skipped = true
			return result
		}
		hashMap[localHash] = hash
		result.RedactedBlobs[hash] = redacted
		objects = append(objects, syncObject{
			Kind:      "step_provenance",
			Hash:      hash,
			SizeBytes: len(redacted),
		})
	}

	// Rewrite bundle's embedded hashes to upload hashes, then redact/hash.
	rewrittenBundle := RewriteBundleHashes(rawBundle, hashMap)
	bundleHash, bundleRedacted, err := DeriveUploadHash(rewrittenBundle, "bundle", repoPath)
	if err != nil {
		markFailed(ctx, h, m.ManifestID, "bundle redaction failed: "+err.Error())
		result.Skipped = true
		return result
	}
	result.RedactedBlobs[bundleHash] = bundleRedacted
	objects = append(objects, syncObject{Kind: "bundle", Hash: bundleHash, SizeBytes: len(bundleRedacted)})

	result.ObjectCount = len(objects)

	// Build envelope.
	sessID, sessModel := sessionMeta(ctx, h, m)
	env := syncEnvelope{
		ConnectedRepoID:   settings.ConnectedRepoID,
		Provider:          m.Provider,
		ProviderSessionID: sessID,
		TurnID:            m.TurnID,
		Model:             sessModel,
		StartedAt:         m.StartedAt,
		Objects:           objects,
	}
	if m.CompletedAt.Valid {
		env.CompletedAt = m.CompletedAt.Int64
	}

	// Resolve commit linkage by finding the earliest checkpoint in this session
	// created at or after the manifest's start time, then checking whether that
	// checkpoint has a commit link. No row means the turn has no commit link.
	// This is anchored on started_at because turns are expected to complete
	// before their covering checkpoint. If turns can cross checkpoint
	// boundaries, switch this to completed_at or a coalesced fallback.
	link, err := h.Queries.GetManifestCommitLink(ctx, sqldb.GetManifestCommitLinkParams{
		SessionID: m.SessionID,
		CreatedAt: m.StartedAt,
	})
	if err == nil {
		env.CommitHash = link.CommitHash
		env.CheckpointID = link.CheckpointID
	}

	envJSON, err := json.Marshal(env)
	if err != nil {
		markFailed(ctx, h, m.ManifestID, "marshal envelope: "+err.Error())
		result.Skipped = true
		return result
	}
	result.Envelope = envJSON

	return result
}

func loadAndRedact(ctx context.Context, bs *blobs.Store, localHash, kind, repoRoot string) (uploadHash string, redacted []byte, err error) {
	raw, err := bs.Get(ctx, localHash)
	if err != nil {
		return "", nil, err
	}
	return DeriveUploadHash(raw, kind, repoRoot)
}

func markFailed(ctx context.Context, h *sqlstore.Handle, manifestID, reason string) {
	if err := h.Queries.MarkManifestFailed(ctx, sqldb.MarkManifestFailedParams{
		LastError:  sqlstore.NullStr(reason),
		UpdatedAt:  time.Now().UnixMilli(),
		ManifestID: manifestID,
	}); err != nil {
		slog.Warn("sync: mark manifest failed", "manifest_id", manifestID, "err", err)
	}
}

// extractPromptHashFromBytes extracts the prompt blob hash from raw bundle bytes.
func extractPromptHashFromBytes(bundleBytes []byte) string {
	var bundle struct {
		Prompt *struct {
			BlobHash string `json:"blob_hash"`
		} `json:"prompt"`
	}
	if json.Unmarshal(bundleBytes, &bundle) != nil || bundle.Prompt == nil {
		return ""
	}
	return bundle.Prompt.BlobHash
}

// extractStepProvenanceHashes extracts unique non-empty provenance hashes
// from the bundle's steps array.
func extractStepProvenanceHashes(bundleBytes []byte) []string {
	var bundle struct {
		Steps []struct {
			ProvenanceHash string `json:"provenance_hash"`
		} `json:"steps"`
	}
	if json.Unmarshal(bundleBytes, &bundle) != nil {
		return nil
	}
	seen := make(map[string]bool)
	var hashes []string
	for _, s := range bundle.Steps {
		if s.ProvenanceHash == "" || seen[s.ProvenanceHash] {
			continue
		}
		seen[s.ProvenanceHash] = true
		hashes = append(hashes, s.ProvenanceHash)
	}
	return hashes
}

// sessionMeta resolves the provider session ID and model from the manifest's session.
func sessionMeta(ctx context.Context, h *sqlstore.Handle, m sqldb.ProvenanceManifest) (providerSessionID, model string) {
	sess, err := h.Queries.GetAgentSessionByID(ctx, m.SessionID)
	if err != nil {
		return "", ""
	}
	mdl := ""
	if sess.Model.Valid {
		mdl = sess.Model.String
	}
	return sess.ProviderSessionID, mdl
}

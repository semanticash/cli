package provenance

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	claudeagent "github.com/semanticash/cli/internal/agents/claude"
	"github.com/semanticash/cli/internal/redact"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// responseObjectVersion identifies the stored response schema.
const responseObjectVersion = 1

// responseSummaryLimit is the maximum summary length in runes.
const responseSummaryLimit = 200

const (
	responseComplete    = "complete"
	responseEmpty       = "empty"
	responseMissing     = "missing"
	responseUnsupported = "unsupported"
)

// turnResponse is the redacted response object stored in the blob store.
type turnResponse struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	Text    string `json:"text"`
}

// ResponseCandidate identifies a redacted final response.
type ResponseCandidate struct {
	Status      string
	EventID     string
	Hash        string
	Summary     string
	CompletedAt int64
}

// isClaudeProvider reports whether transcript response extraction is supported.
func isClaudeProvider(provider string) bool {
	return provider == "claude_code" || provider == "claude-code"
}

// hookResponseProvider reports whether a provider uses hook-native responses.
func hookResponseProvider(provider string) bool {
	switch provider {
	case "codex", "cursor":
		return true
	}
	return false
}

// RedactAndStoreResponse stores a redacted response and returns its metadata.
// Raw text is never written to the content-addressed store.
func RedactAndStoreResponse(ctx context.Context, bs *blobs.Store, eventID, text string, completedAt int64) ResponseCandidate {
	cand := ResponseCandidate{EventID: eventID, CompletedAt: completedAt}
	redacted, err := redact.String(text)
	if err != nil {
		slog.Warn("provenance: redact response failed", "err", err)
		return withStatus(cand, responseMissing)
	}
	if redacted == "" {
		return withStatus(cand, responseEmpty)
	}
	obj := turnResponse{Version: responseObjectVersion, Kind: "turn_response", Text: redacted}
	data, err := json.Marshal(obj)
	if err != nil {
		return withStatus(cand, responseMissing)
	}
	hash, _, err := bs.Put(ctx, data)
	if err != nil {
		slog.Debug("provenance: store response object failed", "err", err)
		return withStatus(cand, responseMissing)
	}
	cand.Hash = hash
	cand.Summary = summarizeResponse(redacted)
	cand.Status = responseComplete
	return cand
}

// captureFinalResponse prefers hook evidence, then uses supported fallbacks.
func captureFinalResponse(ctx context.Context, h *sqlstore.Handle, bs *blobs.Store, provider, sessionID, turnID string, candidate ResponseCandidate) ResponseCandidate {
	if candidate.Status != "" {
		return candidate
	}
	if isClaudeProvider(provider) {
		return captureClaudeTranscriptResponse(ctx, h, bs, sessionID, turnID)
	}
	if hookResponseProvider(provider) {
		return ResponseCandidate{Status: responseMissing}
	}
	return ResponseCandidate{Status: responseUnsupported}
}

// captureClaudeTranscriptResponse stores Claude's final transcript response.
func captureClaudeTranscriptResponse(ctx context.Context, h *sqlstore.Handle, bs *blobs.Store, sessionID, turnID string) ResponseCandidate {
	row, err := h.Queries.GetFinalAssistantEventForTurn(ctx, sqldb.GetFinalAssistantEventForTurnParams{
		SessionID: sessionID,
		TurnID:    sqlstore.NullStr(turnID),
	})
	if err != nil {
		return ResponseCandidate{Status: responseMissing}
	}
	res := ResponseCandidate{EventID: row.EventID, CompletedAt: row.Ts}
	if !row.PayloadHash.Valid || row.PayloadHash.String == "" {
		return withStatus(res, responseMissing)
	}
	raw, err := bs.Get(ctx, row.PayloadHash.String)
	if err != nil {
		slog.Debug("provenance: load response payload failed", "err", err)
		return withStatus(res, responseMissing)
	}
	text, err := claudeagent.FinalAssistantText(string(raw))
	if err != nil {
		slog.Debug("provenance: extract response text failed", "err", err)
		return withStatus(res, responseMissing)
	}
	return RedactAndStoreResponse(ctx, bs, row.EventID, text, row.Ts)
}

// ensureResponseResolvable keeps a hash only when its object exists in the
// repository store or can be copied there with the same hash.
func ensureResponseResolvable(ctx context.Context, bs, sourceBlobs *blobs.Store, r ResponseCandidate) ResponseCandidate {
	if r.Hash == "" {
		return r
	}
	if _, err := bs.Get(ctx, r.Hash); err == nil {
		return r // already in the repository store
	}
	if sourceBlobs != nil {
		if raw, err := sourceBlobs.Get(ctx, r.Hash); err == nil {
			if h, _, perr := bs.Put(ctx, raw); perr == nil && h == r.Hash {
				return r // copied and verified
			}
		}
	}
	slog.Debug("provenance: response object unresolvable in repo store, recording missing", "hash", r.Hash)
	return ResponseCandidate{Status: responseMissing, EventID: r.EventID, CompletedAt: r.CompletedAt}
}

func withStatus(r ResponseCandidate, status string) ResponseCandidate {
	r.Status = status
	return r
}

// summarizeResponse returns a bounded single-line summary.
func summarizeResponse(text string) string {
	text = strings.ReplaceAll(text, "\r", "")
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) > responseSummaryLimit {
		return string(runes[:responseSummaryLimit])
	}
	return text
}

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

// capturedResponse contains response metadata stored on a turn manifest.
type capturedResponse struct {
	Status      string
	EventID     string
	Hash        string
	Summary     string
	CompletedAt int64
}

// responseSupported reports whether response extraction is available.
func responseSupported(provider string) bool {
	return provider == "claude_code" || provider == "claude-code"
}

// captureFinalResponse stores the turn's final visible response after redaction.
func captureFinalResponse(ctx context.Context, h *sqlstore.Handle, bs *blobs.Store, provider, sessionID, turnID string) capturedResponse {
	if !responseSupported(provider) {
		return capturedResponse{Status: responseUnsupported}
	}

	row, err := h.Queries.GetFinalAssistantEventForTurn(ctx, sqldb.GetFinalAssistantEventForTurnParams{
		SessionID: sessionID,
		TurnID:    sqlstore.NullStr(turnID),
	})
	if err != nil {
		return capturedResponse{Status: responseMissing}
	}

	res := capturedResponse{EventID: row.EventID, CompletedAt: row.Ts}

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
	redacted, err := redact.String(text)
	if err != nil {
		slog.Warn("provenance: redact response failed", "err", err)
		return withStatus(res, responseMissing)
	}
	if redacted == "" {
		return withStatus(res, responseEmpty)
	}

	obj := turnResponse{Version: responseObjectVersion, Kind: "turn_response", Text: redacted}
	data, err := json.Marshal(obj)
	if err != nil {
		return withStatus(res, responseMissing)
	}
	hash, _, err := bs.Put(ctx, data)
	if err != nil {
		slog.Debug("provenance: store response object failed", "err", err)
		return withStatus(res, responseMissing)
	}

	res.Hash = hash
	res.Summary = summarizeResponse(redacted)
	res.Status = responseComplete
	return res
}

func withStatus(r capturedResponse, status string) capturedResponse {
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

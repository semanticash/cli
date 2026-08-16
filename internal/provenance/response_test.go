package provenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// responseWorld provides a migrated database and blob store.
type responseWorld struct {
	h         *sqlstore.Handle
	bs        *blobs.Store
	repoID    string
	sessionID string
}

func newResponseWorld(t *testing.T, provider string) *responseWorld {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlstore.Close(h) })
	bs, err := blobs.NewStore(filepath.Join(dir, "objects"))
	if err != nil {
		t.Fatal(err)
	}

	repoID := uuid.NewString()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: dir, CreatedAt: 1, EnabledAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	srcRow, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: uuid.NewString(), RepositoryID: repoID, Provider: provider,
		SourceKey: "/fake.jsonl", LastSeenAt: 1, CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessRow, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: uuid.NewString(), ProviderSessionID: "psess", RepositoryID: repoID,
		SourceID: srcRow.SourceID, Provider: provider, StartedAt: 1, LastSeenAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &responseWorld{h: h, bs: bs, repoID: repoID, sessionID: sessRow.SessionID}
}

// addAssistantEvent inserts a transcript-sourced Claude assistant event.
func (w *responseWorld) addAssistantEvent(t *testing.T, turnID string, ts int64, content []map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"type":    "assistant",
		"message": map[string]any{"role": "assistant", "content": content},
	})
	if err != nil {
		t.Fatal(err)
	}
	return w.addAssistantRaw(t, turnID, ts, "transcript", payload)
}

// addAssistantRaw inserts an assistant event with the supplied source and payload.
func (w *responseWorld) addAssistantRaw(t *testing.T, turnID string, ts int64, source string, payload []byte) string {
	t.Helper()
	ctx := context.Background()
	hash, _, err := w.bs.Put(ctx, payload)
	if err != nil {
		t.Fatal(err)
	}
	eventID := uuid.NewString()
	if err := w.h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: eventID, SessionID: w.sessionID, RepositoryID: w.repoID, Ts: ts,
		Kind: "assistant", Role: sqlstore.NullStr("assistant"),
		TurnID: sqlstore.NullStr(turnID), PayloadHash: sqlstore.NullStr(hash),
		EventSource: source,
	}); err != nil {
		t.Fatal(err)
	}
	return eventID
}

func textBlock(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

// upsertManifestResponse writes response metadata through the manifest upsert.
func (w *responseWorld) upsertManifestResponse(t *testing.T, turnID string, r capturedResponse) {
	t.Helper()
	if err := w.h.Queries.UpsertProvenanceManifest(context.Background(), sqldb.UpsertProvenanceManifestParams{
		ManifestID: uuid.NewString(), RepositoryID: w.repoID, SessionID: w.sessionID,
		TurnID: turnID, Provider: "claude_code", Kind: "turn_bundle",
		StartedAt: 1, Status: "packaged",
		ResponseEventID:     sqlstore.NullStr(r.EventID),
		ResponseHash:        sqlstore.NullStr(r.Hash),
		ResponseSummary:     sqlstore.NullStr(r.Summary),
		ResponseStatus:      sqlstore.NullStr(r.Status),
		ResponseCompletedAt: sql.NullInt64{Int64: r.CompletedAt, Valid: r.CompletedAt > 0},
		CreatedAt:           1, UpdatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

func (w *responseWorld) readManifestResponse(t *testing.T, turnID string) capturedResponse {
	t.Helper()
	var r capturedResponse
	var status, hash, summary, eventID sql.NullString
	var completedAt sql.NullInt64
	row := w.h.DB.QueryRowContext(context.Background(),
		`select response_status, response_hash, response_summary, response_event_id, response_completed_at
		 from provenance_manifests where session_id = ? and turn_id = ? and kind = 'turn_bundle'`,
		w.sessionID, turnID)
	if err := row.Scan(&status, &hash, &summary, &eventID, &completedAt); err != nil {
		t.Fatal(err)
	}
	r.Status, r.Hash, r.Summary, r.EventID = status.String, hash.String, summary.String, eventID.String
	r.CompletedAt = completedAt.Int64
	return r
}

func TestUpsertManifest_FailedRetryPreservesCompleteResponse(t *testing.T) {
	w := newResponseWorld(t, "claude_code")
	want := capturedResponse{
		Status: responseComplete, EventID: "evt-1", Hash: "hash-1",
		Summary: "the response", CompletedAt: 100,
	}
	w.upsertManifestResponse(t, "turn-x", want)
	w.upsertManifestResponse(t, "turn-x", capturedResponse{Status: responseMissing})

	got := w.readManifestResponse(t, "turn-x")
	if got != want {
		t.Errorf("after failed retry: got %+v, want complete evidence preserved %+v", got, want)
	}
}

func TestUpsertManifest_SuccessfulRetrySupersedes(t *testing.T) {
	w := newResponseWorld(t, "claude_code")
	w.upsertManifestResponse(t, "turn-y", capturedResponse{Status: responseMissing})
	want := capturedResponse{
		Status: responseComplete, EventID: "evt-2", Hash: "hash-2", Summary: "recovered", CompletedAt: 200,
	}
	w.upsertManifestResponse(t, "turn-y", want)

	if got := w.readManifestResponse(t, "turn-y"); got != want {
		t.Errorf("after successful retry: got %+v, want the new complete evidence %+v", got, want)
	}
}

func TestCaptureFinalResponse_Complete(t *testing.T) {
	w := newResponseWorld(t, "claude_code")
	ctx := context.Background()
	w.addAssistantEvent(t, "turn-1", 100, []map[string]any{
		{"type": "tool_use", "id": "toolu_1", "name": "Edit"},
	})
	lastID := w.addAssistantEvent(t, "turn-1", 200, []map[string]any{
		textBlock("Implemented the requested changes and ran the tests."),
	})

	got := captureFinalResponse(ctx, w.h, w.bs, "claude_code", w.sessionID, "turn-1")
	if got.Status != responseComplete {
		t.Fatalf("status = %q, want complete", got.Status)
	}
	if got.EventID != lastID {
		t.Errorf("event = %q, want last assistant event %q", got.EventID, lastID)
	}
	if got.CompletedAt != 200 {
		t.Errorf("completedAt = %d, want 200", got.CompletedAt)
	}
	if got.Summary != "Implemented the requested changes and ran the tests." {
		t.Errorf("summary = %q", got.Summary)
	}
	raw, err := w.bs.Get(ctx, got.Hash)
	if err != nil {
		t.Fatal(err)
	}
	var obj turnResponse
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	if obj.Version != responseObjectVersion || obj.Kind != "turn_response" {
		t.Errorf("object = %+v, want version %d kind turn_response", obj, responseObjectVersion)
	}
	if obj.Text != "Implemented the requested changes and ran the tests." {
		t.Errorf("object text = %q", obj.Text)
	}
}

func TestCaptureFinalResponse_EmptyWhenNoVisibleText(t *testing.T) {
	w := newResponseWorld(t, "claude_code")
	ctx := context.Background()
	id := w.addAssistantEvent(t, "turn-2", 100, []map[string]any{
		{"type": "tool_use", "id": "toolu_1", "name": "Bash"},
	})

	got := captureFinalResponse(ctx, w.h, w.bs, "claude_code", w.sessionID, "turn-2")
	if got.Status != responseEmpty {
		t.Fatalf("status = %q, want empty", got.Status)
	}
	if got.EventID != id {
		t.Errorf("event = %q, want %q", got.EventID, id)
	}
	if got.Hash != "" || got.Summary != "" {
		t.Errorf("empty response stored a body: hash=%q summary=%q", got.Hash, got.Summary)
	}
}

func TestCaptureFinalResponse_MissingWhenNoAssistantEvent(t *testing.T) {
	w := newResponseWorld(t, "claude_code")
	got := captureFinalResponse(context.Background(), w.h, w.bs, "claude_code", w.sessionID, "turn-none")
	if got.Status != responseMissing {
		t.Fatalf("status = %q, want missing", got.Status)
	}
	if got.EventID != "" || got.Hash != "" {
		t.Errorf("missing response carried refs: %+v", got)
	}
}

func TestCaptureFinalResponse_IgnoresHookAssistantRow(t *testing.T) {
	w := newResponseWorld(t, "claude_code")
	ctx := context.Background()
	transcriptID := w.addAssistantEvent(t, "turn-h", 100, []map[string]any{
		textBlock("The real final response."),
	})
	w.addAssistantRaw(t, "turn-h", 200, "hook", []byte(`{"synthesized":true}`))

	got := captureFinalResponse(ctx, w.h, w.bs, "claude_code", w.sessionID, "turn-h")
	if got.Status != responseComplete {
		t.Fatalf("status = %q, want complete", got.Status)
	}
	if got.EventID != transcriptID {
		t.Errorf("event = %q, want the transcript event %q", got.EventID, transcriptID)
	}
}

func TestCaptureFinalResponse_EqualTimestampUsesSequence(t *testing.T) {
	w := newResponseWorld(t, "claude_code")
	ctx := context.Background()
	w.addAssistantEvent(t, "turn-eq", 100, []map[string]any{textBlock("first")})
	w.addAssistantEvent(t, "turn-eq", 100, []map[string]any{textBlock("second and final")})

	got := captureFinalResponse(ctx, w.h, w.bs, "claude_code", w.sessionID, "turn-eq")
	if got.Status != responseComplete || got.Summary != "second and final" {
		t.Fatalf("got status=%q summary=%q, want the last-inserted row", got.Status, got.Summary)
	}
}

func TestCaptureFinalResponse_MalformedPayloadIsMissing(t *testing.T) {
	w := newResponseWorld(t, "claude_code")
	ctx := context.Background()
	id := w.addAssistantRaw(t, "turn-bad", 100, "transcript", []byte(`{"message": not json`))

	got := captureFinalResponse(ctx, w.h, w.bs, "claude_code", w.sessionID, "turn-bad")
	if got.Status != responseMissing {
		t.Fatalf("status = %q, want missing for malformed payload", got.Status)
	}
	if got.EventID != id {
		t.Errorf("event = %q, want %q", got.EventID, id)
	}
	if got.Hash != "" {
		t.Errorf("malformed payload stored a body: %q", got.Hash)
	}
}

func TestCaptureFinalResponse_UnsupportedProvider(t *testing.T) {
	w := newResponseWorld(t, "gemini_cli")
	w.addAssistantEvent(t, "turn-1", 100, []map[string]any{textBlock("done")})
	got := captureFinalResponse(context.Background(), w.h, w.bs, "gemini_cli", w.sessionID, "turn-1")
	if got.Status != responseUnsupported {
		t.Fatalf("status = %q, want unsupported", got.Status)
	}
	if got.Hash != "" || got.EventID != "" {
		t.Errorf("unsupported provider produced evidence: %+v", got)
	}
}

func TestCaptureFinalResponse_RedactsBeforeStorage(t *testing.T) {
	w := newResponseWorld(t, "claude_code")
	ctx := context.Background()
	// Use an assignment form recognized by the redactor.
	secret := "sk-1234567890abcdef1234567890abcdef"
	w.addAssistantEvent(t, "turn-r", 100, []map[string]any{
		textBlock("Set api_key = " + secret + " in the config."),
	})

	got := captureFinalResponse(ctx, w.h, w.bs, "claude_code", w.sessionID, "turn-r")
	if got.Status != responseComplete {
		t.Fatalf("status = %q, want complete", got.Status)
	}
	raw, err := w.bs.Get(ctx, got.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Error("stored response object contains an unredacted secret")
	}
	if strings.Contains(got.Summary, secret) {
		t.Error("summary contains an unredacted secret")
	}
}

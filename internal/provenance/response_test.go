package provenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
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
func (w *responseWorld) upsertManifestResponse(t *testing.T, turnID string, r ResponseCandidate) {
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

func (w *responseWorld) readManifestResponse(t *testing.T, turnID string) ResponseCandidate {
	t.Helper()
	var r ResponseCandidate
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
	want := ResponseCandidate{
		Status: responseComplete, EventID: "evt-1", Hash: "hash-1",
		Summary: "the response", CompletedAt: 100,
	}
	w.upsertManifestResponse(t, "turn-x", want)
	w.upsertManifestResponse(t, "turn-x", ResponseCandidate{Status: responseMissing})

	got := w.readManifestResponse(t, "turn-x")
	if got != want {
		t.Errorf("after failed retry: got %+v, want complete evidence preserved %+v", got, want)
	}
}

func TestUpsertManifest_SuccessfulRetrySupersedes(t *testing.T) {
	w := newResponseWorld(t, "claude_code")
	w.upsertManifestResponse(t, "turn-y", ResponseCandidate{Status: responseMissing})
	want := ResponseCandidate{
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

	got := captureFinalResponse(ctx, w.h, w.bs, "claude_code", w.sessionID, "turn-1", ResponseCandidate{})
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

	got := captureFinalResponse(ctx, w.h, w.bs, "claude_code", w.sessionID, "turn-2", ResponseCandidate{})
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
	got := captureFinalResponse(context.Background(), w.h, w.bs, "claude_code", w.sessionID, "turn-none", ResponseCandidate{})
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

	got := captureFinalResponse(ctx, w.h, w.bs, "claude_code", w.sessionID, "turn-h", ResponseCandidate{})
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

	got := captureFinalResponse(ctx, w.h, w.bs, "claude_code", w.sessionID, "turn-eq", ResponseCandidate{})
	if got.Status != responseComplete || got.Summary != "second and final" {
		t.Fatalf("got status=%q summary=%q, want the last-inserted row", got.Status, got.Summary)
	}
}

func TestCaptureFinalResponse_MalformedPayloadIsMissing(t *testing.T) {
	w := newResponseWorld(t, "claude_code")
	ctx := context.Background()
	id := w.addAssistantRaw(t, "turn-bad", 100, "transcript", []byte(`{"message": not json`))

	got := captureFinalResponse(ctx, w.h, w.bs, "claude_code", w.sessionID, "turn-bad", ResponseCandidate{})
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
	// Gemini CLI and Kiro CLI responses remain unsupported because their
	// completion hooks can continue after firing.
	for _, provider := range []string{"gemini_cli", "gemini-cli", "kiro_cli", "kiro-cli"} {
		t.Run(provider, func(t *testing.T) {
			w := newResponseWorld(t, provider)
			w.addAssistantEvent(t, "turn-1", 100, []map[string]any{textBlock("done")})
			got := captureFinalResponse(context.Background(), w.h, w.bs, provider, w.sessionID, "turn-1", ResponseCandidate{})
			if got.Status != responseUnsupported {
				t.Fatalf("status = %q, want unsupported", got.Status)
			}
			if got.Hash != "" || got.EventID != "" {
				t.Errorf("unsupported provider produced evidence: %+v", got)
			}
		})
	}
}

func TestRedactAndStoreResponse(t *testing.T) {
	w := newResponseWorld(t, "codex")
	ctx := context.Background()

	cand := RedactAndStoreResponse(ctx, w.bs, "evt-1", "the final answer", 500)
	if cand.Status != responseComplete || cand.Hash == "" {
		t.Fatalf("candidate = %+v, want complete with hash", cand)
	}
	if cand.EventID != "evt-1" || cand.CompletedAt != 500 || cand.Summary == "" {
		t.Errorf("candidate metadata = %+v", cand)
	}
	raw, err := w.bs.Get(ctx, cand.Hash)
	if err != nil {
		t.Fatal(err)
	}
	var obj turnResponse
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("stored object is not a turn_response: %v", err)
	}
	if obj.Kind != "turn_response" || obj.Version != responseObjectVersion || obj.Text != "the final answer" {
		t.Errorf("stored object = %+v", obj)
	}

	// Blank text yields empty with no blob.
	empty := RedactAndStoreResponse(ctx, w.bs, "evt-2", "", 600)
	if empty.Status != responseEmpty || empty.Hash != "" {
		t.Errorf("blank = %+v, want empty with no hash", empty)
	}

	// Raw secrets never reach the store: the redactor runs before Put.
	secret := "sk-1234567890abcdef1234567890abcdef"
	red := RedactAndStoreResponse(ctx, w.bs, "evt-3", "api_key = "+secret, 700)
	if red.Status != responseComplete {
		t.Fatalf("status = %q, want complete", red.Status)
	}
	stored, err := w.bs.Get(ctx, red.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), secret) {
		t.Error("raw secret reached the content-addressed store")
	}
}

// Hook-provided responses take precedence over transcript extraction.
func TestCaptureFinalResponse_CandidateWins(t *testing.T) {
	w := newResponseWorld(t, "codex")
	cand := ResponseCandidate{Status: responseComplete, Hash: "h1", Summary: "hi", EventID: "e1", CompletedAt: 700}
	got := captureFinalResponse(context.Background(), w.h, w.bs, "codex", w.sessionID, "turn-x", cand)
	if got != cand {
		t.Errorf("got %+v, want candidate %+v", got, cand)
	}
}

// A hook provider without a response does not use Claude transcript extraction.
func TestCaptureFinalResponse_HookProviderNoCandidateMissing(t *testing.T) {
	for _, provider := range []string{"codex", "cursor"} {
		t.Run(provider, func(t *testing.T) {
			w := newResponseWorld(t, provider)
			// Hook-native providers do not fall back to transcript extraction.
			w.addAssistantEvent(t, "turn-x", 100, []map[string]any{textBlock("done")})
			got := captureFinalResponse(context.Background(), w.h, w.bs, provider, w.sessionID, "turn-x", ResponseCandidate{})
			if got.Status != responseMissing {
				t.Errorf("status = %q, want missing", got.Status)
			}
			if got.Hash != "" {
				t.Errorf("%s without candidate produced a hash: %+v", provider, got)
			}
		})
	}
}

// Unresolvable response objects are recorded as missing.
func TestEnsureResponseResolvable(t *testing.T) {
	ctx := context.Background()
	newStore := func() *blobs.Store {
		s, err := blobs.NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	put := func(s *blobs.Store, text string) string {
		return RedactAndStoreResponse(ctx, s, "", text, 1).Hash
	}
	complete := func(hash string) ResponseCandidate {
		return ResponseCandidate{Status: responseComplete, Hash: hash, Summary: "s", CompletedAt: 1}
	}

	t.Run("already in repo store", func(t *testing.T) {
		bs := newStore()
		h := put(bs, "answer")
		if got := ensureResponseResolvable(ctx, bs, nil, complete(h)); got.Status != responseComplete || got.Hash != h {
			t.Errorf("got %+v, want kept", got)
		}
	})

	t.Run("copied from source store", func(t *testing.T) {
		bs, src := newStore(), newStore()
		h := put(src, "answer")
		got := ensureResponseResolvable(ctx, bs, src, complete(h))
		if got.Status != responseComplete || got.Hash != h {
			t.Fatalf("got %+v, want kept", got)
		}
		if _, err := bs.Get(ctx, h); err != nil {
			t.Errorf("object not copied into repo store: %v", err)
		}
	})

	t.Run("missing source object", func(t *testing.T) {
		bs, src := newStore(), newStore()
		got := ensureResponseResolvable(ctx, bs, src, complete("deadbeef"))
		if got.Status != responseMissing || got.Hash != "" {
			t.Errorf("got %+v, want missing with no hash", got)
		}
	})

	t.Run("nil source store", func(t *testing.T) {
		bs := newStore()
		got := ensureResponseResolvable(ctx, bs, nil, complete("deadbeef"))
		if got.Status != responseMissing || got.Hash != "" {
			t.Errorf("got %+v, want missing with no hash", got)
		}
	})

	t.Run("failed destination write", func(t *testing.T) {
		src := newStore()
		h := put(src, "answer")
		// A repo store rooted at a regular file makes Put (and Get) fail.
		badRoot := filepath.Join(t.TempDir(), "notadir")
		if err := os.WriteFile(badRoot, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		bad, err := blobs.NewStore(badRoot)
		if err != nil {
			t.Fatal(err)
		}
		got := ensureResponseResolvable(ctx, bad, src, complete(h))
		if got.Status != responseMissing || got.Hash != "" {
			t.Errorf("got %+v, want missing with no hash", got)
		}
	})

	t.Run("empty candidate untouched", func(t *testing.T) {
		bs := newStore()
		empty := ResponseCandidate{Status: responseEmpty}
		if got := ensureResponseResolvable(ctx, bs, nil, empty); got != empty {
			t.Errorf("got %+v, want %+v", got, empty)
		}
	})
}

// Packaging copies hook-provided response objects into the repository store.
func TestPackageTurn_HookCandidateResolvableInRepoStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	semDir := filepath.Join(dir, ".semantica")
	if err := os.MkdirAll(filepath.Join(semDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(semDir, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	repoID := uuid.NewString()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: dir, CreatedAt: 1, EnabledAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	src, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: uuid.NewString(), RepositoryID: repoID, Provider: "codex",
		SourceKey: "/f.jsonl", LastSeenAt: 1, CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: uuid.NewString(), ProviderSessionID: "ps", RepositoryID: repoID,
		SourceID: src.SourceID, Provider: "codex", StartedAt: 1, LastSeenAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	_ = sqlstore.Close(h) // release the db before PackageTurn opens its own handle

	// The candidate object lives only in the global (source) store.
	global, err := blobs.NewStore(filepath.Join(t.TempDir(), "global"))
	if err != nil {
		t.Fatal(err)
	}
	cand := RedactAndStoreResponse(ctx, global, "", "the final answer", 500)
	if cand.Status != responseComplete || cand.Hash == "" {
		t.Fatalf("candidate = %+v", cand)
	}
	repoStore, err := blobs.NewStore(filepath.Join(semDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repoStore.Get(ctx, cand.Hash); err == nil {
		t.Fatal("candidate unexpectedly already present in the repo store")
	}

	PackageTurn(ctx, dir, TurnContext{
		TurnID: "turn-1", SessionID: "ps", Provider: "codex", CWD: dir,
		StartedAt: 1, CompletedAt: 2, ResponseCandidate: cand,
	}, global)

	// The redacted object now resolves in the repository store.
	if _, err := repoStore.Get(ctx, cand.Hash); err != nil {
		t.Errorf("response object not copied into repo store: %v", err)
	}
	// The manifest references it.
	h2, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h2) }()
	var hash, status string
	if err := h2.DB.QueryRowContext(ctx,
		"select response_hash, response_status from provenance_manifests where turn_id = ?", "turn-1").
		Scan(&hash, &status); err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if hash != cand.Hash || status != responseComplete {
		t.Errorf("manifest response = (%q, %q), want (%q, complete)", hash, status, cand.Hash)
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

	got := captureFinalResponse(ctx, w.h, w.bs, "claude_code", w.sessionID, "turn-r", ResponseCandidate{})
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

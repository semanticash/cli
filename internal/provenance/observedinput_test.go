package provenance

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	claudeagent "github.com/semanticash/cli/internal/agents/claude"
	"github.com/semanticash/cli/internal/observedinput"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func newObservedRepo(t *testing.T) (repoPath, providerSession string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	sem := filepath.Join(dir, ".semantica")
	if err := os.MkdirAll(sem, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sem, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(sem, "lineage.db")
	if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
		t.Fatal(err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	repoID := uuid.NewString()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: dir, CreatedAt: 1, EnabledAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	src, err := h.Queries.UpsertAgentSource(ctx, sqldb.UpsertAgentSourceParams{
		SourceID: uuid.NewString(), RepositoryID: repoID, Provider: "claude_code",
		SourceKey: "observed-input:sess", LastSeenAt: 1, CreatedAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: uuid.NewString(), ProviderSessionID: "psess", RepositoryID: repoID,
		SourceID: src.SourceID, Provider: "claude_code", StartedAt: 1, LastSeenAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return dir, "psess"
}

func normalizeFixtureForRepo(t *testing.T, name string) claudeagent.Normalized {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "agents", "claude", "testdata", "observedinput", name))
	if err != nil {
		t.Fatal(err)
	}
	var recs []json.RawMessage
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		if len(line) > 0 {
			recs = append(recs, json.RawMessage(line))
		}
	}
	n, err := claudeagent.NormalizeObservedInputs(claudeagent.NormalizeInput{
		Provider: "claude_code", SessionID: "sess", Locator: name, Records: recs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestObservedInput_PersistCollectRoundTrip(t *testing.T) {
	ctx := context.Background()
	repo, ps := newObservedRepo(t)
	n := normalizeFixtureForRepo(t, "captured_pdf_attachment.jsonl")

	if err := PersistObservedInputs(ctx, repo, ps, 1000, n.Turns, n.Contents); err != nil {
		t.Fatalf("persist: %v", err)
	}

	turn := n.Turns[0].TurnID
	ev, hash, err := CollectObservedInput(ctx, repo, "claude_code", "sess", turn)
	if err != nil || ev == nil {
		t.Fatalf("collect: ev=%v hash=%q err=%v", ev, hash, err)
	}
	if len(ev.Requests) != 1 {
		t.Fatalf("collected requests = %d, want 1", len(ev.Requests))
	}
	// Validate the collected document.
	if err := observedinput.Validate(*ev); err != nil {
		t.Fatalf("collected evidence invalid: %v", err)
	}
}

func TestObservedInput_ReplayIsIdempotent(t *testing.T) {
	ctx := context.Background()
	repo, ps := newObservedRepo(t)
	n := normalizeFixtureForRepo(t, "captured_pdf_attachment.jsonl")

	if err := PersistObservedInputs(ctx, repo, ps, 1000, n.Turns, n.Contents); err != nil {
		t.Fatal(err)
	}
	_, first, err := CollectObservedInput(ctx, repo, "claude_code", "sess", n.Turns[0].TurnID)
	if err != nil {
		t.Fatal(err)
	}
	// Replay must preserve the document hash.
	if err := PersistObservedInputs(ctx, repo, ps, 1000, n.Turns, n.Contents); err != nil {
		t.Fatalf("replay: %v", err)
	}
	_, second, err := CollectObservedInput(ctx, repo, "claude_code", "sess", n.Turns[0].TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("replay changed the document: %s -> %s", first, second)
	}
}

func TestObservedInput_ConflictingContentDiagnosed(t *testing.T) {
	ctx := context.Background()
	repo, ps := newObservedRepo(t)
	n := normalizeFixtureForRepo(t, "captured_pdf_attachment.jsonl")
	if err := PersistObservedInputs(ctx, repo, ps, 1000, n.Turns, n.Contents); err != nil {
		t.Fatal(err)
	}
	// A conflicting document must not replace existing evidence.
	conflict := n.Turns[0]
	conflict.Gaps = append(conflict.Gaps, observedinput.Gap{Reason: observedinput.GapFailure, Detail: "altered"})
	err := PersistObservedInputs(ctx, repo, ps, 1000, []observedinput.Evidence{conflict}, n.Contents)
	if err == nil {
		t.Fatal("conflicting evidence for the same turn was not diagnosed")
	}
}

func TestObservedInput_MissingTurnReturnsNil(t *testing.T) {
	ctx := context.Background()
	repo, _ := newObservedRepo(t)
	ev, hash, err := CollectObservedInput(ctx, repo, "claude_code", "sess", "no-such-turn")
	if err != nil || ev != nil || hash != "" {
		t.Fatalf("want (nil, \"\", nil) for a turn with no evidence, got ev=%v hash=%q err=%v", ev, hash, err)
	}
}

func TestObservedInput_BundleSectionResolves(t *testing.T) {
	ctx := context.Background()
	repo, ps := newObservedRepo(t)
	n := normalizeFixtureForRepo(t, "captured_pdf_attachment.jsonl")
	if err := PersistObservedInputs(ctx, repo, ps, 1000, n.Turns, n.Contents); err != nil {
		t.Fatal(err)
	}
	bs, err := blobs.NewStore(filepath.Join(repo, ".semantica", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	turn := n.Turns[0].TurnID
	hash, _, err := buildProvenanceBundleFromFiltered(ctx, repo, bs,
		TurnContext{Provider: "claude_code", TurnID: turn}, sqldb.AgentSession{SessionID: "sess"},
		nil, nil, map[string]string{}, ResponseCandidate{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := bs.Get(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	var bundle struct {
		ObservedInput *bundleObservedInput `json:"observed_input"`
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.ObservedInput == nil || bundle.ObservedInput.EvidenceHash == "" {
		t.Fatalf("bundle missing observed_input section: %s", raw)
	}
	// Resolve the document and primary observation content.
	doc, err := bs.Get(ctx, bundle.ObservedInput.EvidenceHash)
	if err != nil {
		t.Fatalf("bundle observed_input evidence unresolved: %v", err)
	}
	var ev observedinput.Evidence
	if err := json.Unmarshal(doc, &ev); err != nil {
		t.Fatal(err)
	}
	for _, o := range ev.Observations {
		if o.Representation.ContentRef != "" && !bs.Exists(o.Representation.ContentRef) {
			t.Fatalf("bundle content %s not resolvable", o.Representation.ContentRef)
		}
	}
}

// Turns without observed-input evidence omit the optional section.
func TestObservedInput_BundleOmitsSectionWhenAbsent(t *testing.T) {
	ctx := context.Background()
	repo, _ := newObservedRepo(t)
	bs, err := blobs.NewStore(filepath.Join(repo, ".semantica", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	hash, _, err := buildProvenanceBundleFromFiltered(ctx, repo, bs,
		TurnContext{Provider: "claude_code", TurnID: "bare"}, sqldb.AgentSession{SessionID: "sess"},
		nil, nil, map[string]string{}, ResponseCandidate{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := bs.Get(ctx, hash)
	if bytes.Contains(raw, []byte("observed_input")) {
		t.Fatalf("empty turn should not carry an observed_input section: %s", raw)
	}
}

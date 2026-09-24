package provenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	claudeagent "github.com/semanticash/cli/internal/agents/claude"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/observedinput"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

type observedRepo struct {
	path, repoID, sessionID, providerSession string
}

func newObservedRepo(t *testing.T) observedRepo {
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
	if err := sqlstore.MigratePath(ctx, filepath.Join(sem, "lineage.db")); err != nil {
		t.Fatal(err)
	}
	h, err := sqlstore.Open(ctx, filepath.Join(sem, "lineage.db"), sqlstore.DefaultOpenOptions())
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
	sess, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: uuid.NewString(), ProviderSessionID: "psess", RepositoryID: repoID,
		SourceID: src.SourceID, Provider: "claude_code", StartedAt: 1, LastSeenAt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return observedRepo{path: dir, repoID: repoID, sessionID: sess.SessionID, providerSession: "psess"}
}

// newDestRepo creates a destination session with no local request evidence.
func newDestRepo(t *testing.T, sourceRepoPath string) observedRepo {
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
	if err := sqlstore.MigratePath(ctx, filepath.Join(sem, "lineage.db")); err != nil {
		t.Fatal(err)
	}
	h, err := sqlstore.Open(ctx, filepath.Join(sem, "lineage.db"), sqlstore.DefaultOpenOptions())
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
	sess, err := h.Queries.UpsertAgentSession(ctx, sqldb.UpsertAgentSessionParams{
		SessionID: uuid.NewString(), ProviderSessionID: "psess", RepositoryID: repoID,
		SourceID: src.SourceID, Provider: "claude_code", StartedAt: 1, LastSeenAt: 1,
		SourceRepoPath: sql.NullString{String: sourceRepoPath, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return observedRepo{path: dir, repoID: repoID, sessionID: sess.SessionID, providerSession: "psess"}
}

// insertPrompt records a request and returns its production turn ID.
func (r observedRepo) insertPrompt(t *testing.T, providerEventID string) string {
	t.Helper()
	ctx := context.Background()
	h, err := sqlstore.Open(ctx, filepath.Join(r.path, ".semantica", "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	turnID := uuid.NewString()
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: uuid.NewString(), SessionID: r.sessionID, RepositoryID: r.repoID, Ts: 1, Kind: "user",
		Role: sql.NullString{String: "user", Valid: true}, ProviderEventID: sql.NullString{String: providerEventID, Valid: true},
		TurnID: sql.NullString{String: turnID, Valid: true}, EventSource: "transcript",
	}); err != nil {
		t.Fatal(err)
	}
	return turnID
}

func normalizeFixtureForRepo(t *testing.T, name string) claudeagent.Normalized {
	t.Helper()
	return normalizeFixtureWithProvider(t, name, "claude_code")
}

func normalizeFixtureWithProvider(t *testing.T, name, provider string) claudeagent.Normalized {
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
		Provider: provider, SessionID: "sess", Locator: name, Records: recs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestObservedInput_ClaudeHookAliasPreservesPackagedTurn(t *testing.T) {
	for _, existingAlias := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing_alias=%t", existingAlias), func(t *testing.T) {
			ctx := context.Background()
			r := newObservedRepo(t)
			n := normalizeFixtureWithProvider(t, "captured_text_attachment.jsonl", "claude-code")
			turnID := r.insertPrompt(t, requestUUID(n))
			bs, err := blobs.NewStore(repoObjects(r.path))
			if err != nil {
				t.Fatal(err)
			}
			promptHash, _, err := bs.Put(ctx, []byte(`{"type":"user","message":{"role":"user","content":"Read the supplied specification."}}`))
			if err != nil {
				t.Fatal(err)
			}
			responseHash, _, err := bs.Put(ctx, []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"The badge values are amber and violet."}]}}`))
			if err != nil {
				t.Fatal(err)
			}
			h, err := sqlstore.Open(ctx, repoLineageDB(r.path), sqlstore.DefaultOpenOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sqlstore.Close(h) }()
			if _, err := h.DB.ExecContext(ctx, "update agent_events set payload_hash=?, event_source='hook' where session_id=? and turn_id=?", promptHash, r.sessionID, turnID); err != nil {
				t.Fatal(err)
			}
			if _, err := broker.WriteEventsToRepo(ctx, r.path, []broker.RawEvent{{
				EventID: "response", SourceKey: "transcript", Provider: "claude_code", ProviderSessionID: r.providerSession,
				TurnID: turnID, Kind: "assistant", Role: "assistant", EventSource: "transcript", Timestamp: 2, PayloadHash: responseHash,
			}}, bs); err != nil {
				t.Fatal(err)
			}
			if existingAlias {
				// Reproduce an evidence event written under the hook alias.
				if _, err := broker.WriteEventsToRepo(ctx, r.path, []broker.RawEvent{{
					EventID: observedInputTurnEventID("claude-code", r.sessionID, turnID), SourceKey: "observed-input:" + r.sessionID,
					Provider: "claude-code", ProviderSessionID: r.providerSession, TurnID: turnID,
					Kind: "context", Role: "system", EventSource: observedInputEvidenceKind, Timestamp: 3,
				}}, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := PersistObservedInputs(ctx, r.path, "claude-code", r.providerSession, 4, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
				t.Fatal(err)
			}
			PackageTurn(ctx, r.path, TurnContext{Provider: "claude-code", SessionID: r.providerSession, TurnID: turnID, StartedAt: 1, CompletedAt: 5}, bs)
			var bundle provenanceBundle
			if err := json.Unmarshal(packagedBundle(t, r, turnID, bs), &bundle); err != nil {
				t.Fatal(err)
			}
			if bundle.SessionID != r.sessionID || bundle.Prompt == nil || bundle.Prompt.BlobHash != promptHash {
				t.Fatalf("packaging lost the canonical session or prompt: %+v", bundle)
			}
			if bundle.Response.Status != responseComplete || bundle.Response.Hash == "" {
				t.Fatalf("packaging lost the response: %+v", bundle.Response)
			}
			if bundle.ObservedInput == nil || bundle.ObservedInput.Unavailable || bundle.ObservedInput.Requests != 1 || bundle.ObservedInput.Observations == 0 {
				t.Fatalf("packaging lost observed inputs: %+v", bundle.ObservedInput)
			}
			raw, err := bs.Get(ctx, bundle.ObservedInput.EvidenceHash)
			if err != nil {
				t.Fatal(err)
			}
			var evidence observedinput.Evidence
			if err := json.Unmarshal(raw, &evidence); err != nil || !closureResolves(ctx, bs, evidence) {
				t.Fatalf("invalid evidence closure: %v", err)
			}
			if !existingAlias {
				var count int
				if err := h.DB.QueryRowContext(ctx, "select count(*) from agent_sessions where provider_session_id=?", r.providerSession).Scan(&count); err != nil || count != 1 {
					t.Fatalf("retention split the session: count=%d err=%v", count, err)
				}
			}
		})
	}
}

// requestUUID returns the first resolved request's provider event ID.
func requestUUID(n claudeagent.Normalized) string {
	for _, ev := range n.Turns {
		if ev.TurnID != "unresolved" && len(ev.Requests) > 0 {
			return ev.Requests[0].ProviderEventID
		}
	}
	return ""
}

func TestObservedInput_PersistCollectRoundTrip(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)
	n := normalizeFixtureForRepo(t, "captured_pdf_attachment.jsonl")
	turnID := r.insertPrompt(t, requestUUID(n))

	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
		t.Fatalf("persist: %v", err)
	}
	res, err := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, turnID)
	if err != nil || res.Evidence == nil {
		t.Fatalf("collect: %+v err=%v", res, err)
	}
	if len(res.Evidence.Requests) != 1 || res.Evidence.TurnID != turnID {
		t.Fatalf("collected evidence wrong: %+v", res.Evidence)
	}
	if err := observedinput.Validate(*res.Evidence); err != nil {
		t.Fatalf("collected evidence invalid: %v", err)
	}
}

func TestObservedInput_SourceFidelitySurvivesPackageTurn(t *testing.T) {
	for _, fixture := range []string{"captured_pdf_attachment.jsonl", "captured_text_attachment.jsonl", "captured_webfetch.jsonl"} {
		t.Run(fixture, func(t *testing.T) {
			ctx := context.Background()
			r := newObservedRepo(t)
			n := normalizeFixtureForRepo(t, fixture)
			turn := r.insertPrompt(t, requestUUID(n))
			if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
				t.Fatal(err)
			}
			bs, err := blobs.NewStore(repoObjects(r.path))
			if err != nil {
				t.Fatal(err)
			}
			PackageTurn(ctx, r.path, TurnContext{Provider: "claude_code", SessionID: r.providerSession, TurnID: turn, StartedAt: 1, CompletedAt: 2000}, bs)
			var bundle struct {
				ObservedInput *bundleObservedInput `json:"observed_input"`
			}
			if err := json.Unmarshal(packagedBundle(t, r, turn, bs), &bundle); err != nil {
				t.Fatal(err)
			}
			if bundle.ObservedInput == nil || bundle.ObservedInput.Unavailable {
				t.Fatalf("missing evidence: %+v", bundle)
			}
			raw, err := bs.Get(ctx, bundle.ObservedInput.EvidenceHash)
			if err != nil {
				t.Fatal(err)
			}
			var got observedinput.Evidence
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			want := map[string]observedinput.ObservedInput{}
			for _, ev := range n.Turns {
				for _, o := range ev.Observations {
					want[o.DeliveryID] = o
				}
			}
			if len(got.Observations) != len(want) {
				t.Fatalf("got %d observations, want %d", len(got.Observations), len(want))
			}
			for _, o := range got.Observations {
				w := want[o.DeliveryID]
				if o.InputSource.Locator == "" || o.InputSource != w.InputSource || !reflect.DeepEqual(o.Representation, w.Representation) {
					t.Fatalf("source/fidelity changed during packaging: got %+v want %+v", o, w)
				}
			}
		})
	}
}

func TestObservedInput_SourceIdentityConflictRejected(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)
	n := normalizeFixtureForRepo(t, "captured_text_attachment.jsonl")
	turn := r.insertPrompt(t, requestUUID(n))
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
		t.Fatal(err)
	}
	want := n.Turns[0].Observations[0].InputSource
	n.Turns[0].Observations[0].InputSource.Locator = "/different/plan.md"
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 2000, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err == nil {
		t.Fatal("changed source accepted for the same delivery")
	}
	got, err := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, turn)
	if err != nil || got.Evidence == nil {
		t.Fatalf("collect: %+v %v", got, err)
	}
	if len(got.Evidence.Observations) != 1 || got.Evidence.Observations[0].InputSource != want {
		t.Fatalf("rejected source became visible: %+v", got.Evidence)
	}
}

func TestObservedInput_ReplayIsIdempotent(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)
	n := normalizeFixtureForRepo(t, "captured_pdf_attachment.jsonl")
	turnID := r.insertPrompt(t, requestUUID(n))

	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
		t.Fatal(err)
	}
	a, _ := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, turnID)
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
		t.Fatalf("replay: %v", err)
	}
	b, _ := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, turnID)
	if a.Hash == "" || a.Hash != b.Hash {
		t.Fatalf("replay changed the assembled document: %s -> %s", a.Hash, b.Hash)
	}
}

// Replaying a request with new inputs must preserve earlier evidence.
func TestObservedInput_IncrementalAccumulation(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)
	n := normalizeFixtureForRepo(t, "captured_pdf_attachment.jsonl")
	turnID := r.insertPrompt(t, requestUUID(n))

	// Persist the request before its inputs arrive.
	reqOnly := n.Turns[0]
	reqOnly.Observations, reqOnly.RequestLinks, reqOnly.ToolCallLinks = nil, nil, nil
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, []observedinput.Evidence{reqOnly}, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
		t.Fatalf("batch1: %v", err)
	}
	// Replay with the attachment and read result.
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
		t.Fatalf("batch2 (accumulation) rejected: %v", err)
	}
	res, err := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, turnID)
	if err != nil || res.Evidence == nil || len(res.Evidence.Observations) == 0 {
		t.Fatalf("accumulated observations missing: %+v err=%v", res, err)
	}
}

func TestObservedInput_ConflictingDeliveryDiagnosed(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)
	n := normalizeFixtureForRepo(t, "captured_pdf_attachment.jsonl")
	r.insertPrompt(t, requestUUID(n))
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
		t.Fatal(err)
	}
	// Change one delivery's content under the same delivery identity.
	conflict := n.Turns[0]
	conflict.Observations = append([]observedinput.ObservedInput(nil), conflict.Observations...)
	for i := range conflict.Observations {
		if conflict.Observations[i].Representation.State == observedinput.RepPresent {
			altered := []byte("tampered")
			h := putContent(n.Contents, altered)
			conflict.Observations[i].Representation.ContentRef = h
			break
		}
	}
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, []observedinput.Evidence{conflict}, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err == nil {
		t.Fatal("changed content for the same delivery was not diagnosed")
	}
}

func TestObservedInput_UnavailableWhenContentMissing(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)
	n := normalizeFixtureForRepo(t, "captured_pdf_attachment.jsonl")
	turnID := r.insertPrompt(t, requestUUID(n))
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
		t.Fatal(err)
	}
	// Remove referenced content to make the evidence unavailable.
	removeOneContentBlob(t, r.path, n)
	res, err := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, turnID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Unavailable || res.Evidence != nil {
		t.Fatalf("missing content should be Unavailable, got %+v", res)
	}
}

func putContent(contents map[string][]byte, b []byte) string {
	sum := sha256.Sum256(b)
	h := hex.EncodeToString(sum[:])
	contents[h] = b
	return h
}

// removeOneContentBlob deletes a referenced content object.
func removeOneContentBlob(t *testing.T, repoPath string, n claudeagent.Normalized) {
	t.Helper()
	var ref string
	for _, ev := range n.Turns {
		for _, o := range ev.Observations {
			if o.Representation.ContentRef != "" {
				ref = o.Representation.ContentRef
			}
		}
	}
	if ref == "" {
		t.Fatal("no content ref to remove")
	}
	objects := filepath.Join(repoPath, ".semantica", "objects")
	removed := false
	_ = filepath.Walk(objects, func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() && strings.Contains(filepath.Base(p), ref) {
			if os.Remove(p) == nil {
				removed = true
			}
		}
		return nil
	})
	if !removed {
		t.Fatalf("could not remove content blob %s", ref)
	}
}

func normalizeBatch(t *testing.T, recs []string) claudeagent.Normalized {
	t.Helper()
	var raw []json.RawMessage
	for _, r := range recs {
		raw = append(raw, json.RawMessage(r))
	}
	n, err := claudeagent.NormalizeObservedInputs(claudeagent.NormalizeInput{
		Provider: "claude_code", SessionID: "sess", Locator: "batch", Records: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Independent batches preserve per-turn ownership through PackageTurn without
// transcript access. Local input evidence must remain excluded from uploads.
func TestObservedInput_AcceptanceThroughPackageTurn(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)

	batch1 := []string{
		`{"type":"user","uuid":"r1","message":{"role":"user","content":"first request"}}`,
		`{"type":"attachment","uuid":"att-r1","parentUuid":"r1","attachment":{"type":"file","filename":"/work/a.txt","content":{"type":"text","file":{"filePath":"/work/a.txt","content":"payload one\n","numLines":1,"totalLines":1}}}}`,
	}
	batch2 := []string{
		`{"type":"user","uuid":"r2","parentUuid":"r1","message":{"role":"user","content":"second request"}}`,
		`{"type":"assistant","uuid":"a2","parentUuid":"r2","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_2","name":"Read"}]}}`,
		`{"type":"user","uuid":"res2","parentUuid":"a2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"read output"}]}}`,
		`{"type":"attachment","uuid":"att-r1-late","parentUuid":"r1","attachment":{"type":"file","filename":"/work/late.txt","content":{"type":"text","file":{"filePath":"/work/late.txt","content":"late payload\n","numLines":1,"totalLines":1}}}}`,
	}

	t1 := r.insertPrompt(t, "r1")
	t2 := r.insertPrompt(t, "r2")

	n1 := normalizeBatch(t, batch1)
	if err := PersistObservedInputs(ctx, r.path, "claude-code", r.providerSession, 1000, n1.Turns, n1.Contents, n1.CallOwners, n1.Ancestry, n1.AttachmentAncestry); err != nil {
		t.Fatalf("persist batch1: %v", err)
	}
	// Normalize the next batch without in-memory state from the first.
	n2 := normalizeBatch(t, batch2)
	if isolatedUnresolved := findBatchTurn(n2, "unresolved"); isolatedUnresolved == nil {
		t.Fatal("expected the late attachment to be unresolved in its isolated batch")
	}
	if err := PersistObservedInputs(ctx, r.path, "claude-code", r.providerSession, 2000, n2.Turns, n2.Contents, n2.CallOwners, n2.Ancestry, n2.AttachmentAncestry); err != nil {
		t.Fatalf("persist batch2: %v", err)
	}

	// The late attachment belongs to T1.
	res1, err := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, t1)
	if err != nil || res1.Evidence == nil {
		t.Fatalf("collect T1: %+v err=%v", res1, err)
	}
	if len(res1.Evidence.Observations) != 2 {
		t.Fatalf("T1 should hold both attachments, got %d: %+v", len(res1.Evidence.Observations), res1.Evidence.Observations)
	}
	for _, o := range res1.Evidence.Observations {
		if o.Scope != observedinput.ScopeRequestEnvelope {
			t.Fatalf("late attachment not promoted to envelope: %+v", o)
		}
	}
	// T2 holds its own request and tool result.
	res2, err := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, t2)
	if err != nil || res2.Evidence == nil || len(res2.Evidence.Observations) != 1 {
		t.Fatalf("collect T2: %+v err=%v", res2, err)
	}

	// Package T1 using durable evidence only.
	bs, err := blobs.NewStore(filepath.Join(r.path, ".semantica", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	PackageTurn(ctx, r.path, TurnContext{Provider: "claude_code", SessionID: r.providerSession, TurnID: t1, StartedAt: 1, CompletedAt: 3000}, bs)

	bundleBytes := packagedBundle(t, r, t1, bs)
	var bundle struct {
		ObservedInput *bundleObservedInput `json:"observed_input"`
	}
	if err := json.Unmarshal(bundleBytes, &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.ObservedInput == nil || bundle.ObservedInput.EvidenceHash == "" || bundle.ObservedInput.Unavailable {
		t.Fatalf("bundle missing resolved observed_input section: %s", bundleBytes)
	}
	doc, err := bs.Get(ctx, bundle.ObservedInput.EvidenceHash)
	if err != nil {
		t.Fatalf("evidence document unresolved: %v", err)
	}
	var ev observedinput.Evidence
	_ = json.Unmarshal(doc, &ev)
	for _, o := range ev.Observations {
		for _, ref := range []string{o.Representation.ContentRef, o.Representation.SourceContentRef} {
			if ref != "" && !bs.Exists(ref) {
				t.Fatalf("content %s not resolvable", ref)
			}
		}
	}
	// Uploads exclude local-only input evidence.
	if !bytes.Contains(bundleBytes, []byte("observed_input")) {
		t.Fatal("precondition: local bundle should contain the section")
	}
	if bytes.Contains(stripObservedInput(bundleBytes), []byte("observed_input")) {
		t.Fatal("observed_input leaked into the upload representation")
	}
}

func findBatchTurn(n claudeagent.Normalized, turnID string) *observedinput.Evidence {
	for i := range n.Turns {
		if n.Turns[i].TurnID == turnID {
			return &n.Turns[i]
		}
	}
	return nil
}

func packagedBundle(t *testing.T, r observedRepo, turnID string, bs *blobs.Store) []byte {
	t.Helper()
	ctx := context.Background()
	h, err := sqlstore.Open(ctx, filepath.Join(r.path, ".semantica", "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	m, err := h.Queries.GetProvenanceManifest(ctx, sqldb.GetProvenanceManifestParams{
		RepositoryID: r.repoID, SessionID: r.sessionID, TurnID: turnID, Kind: "turn_bundle",
	})
	if err != nil || !m.ProvenanceBundleHash.Valid {
		t.Fatalf("no packaged bundle: %v", err)
	}
	raw, err := bs.Get(ctx, m.ProvenanceBundleHash.String)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Missing production turns require a retry.
func TestObservedInput_MissingTurnIsRetryable(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)
	n := normalizeFixtureForRepo(t, "captured_pdf_attachment.jsonl") // no prompt event inserted
	err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry)
	if err == nil || !errors.Is(err, ErrObservedInputRetry) {
		t.Fatalf("missing turn mapping should be retryable, got %v", err)
	}
}

// Resolving ownership must not allow a delivery's content to change.
func TestObservedInput_DeliveryConflictIndependentOfOwnership(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)
	// Retain an attachment before its parent arrives.
	att := []string{
		`{"type":"attachment","uuid":"att-x","parentUuid":"px","attachment":{"type":"file","filename":"/f","content":{"type":"text","file":{"filePath":"/f","content":"one\n","numLines":1,"totalLines":1}}}}`,
	}
	n1 := normalizeBatch(t, att)
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, n1.Turns, n1.Contents, n1.CallOwners, n1.Ancestry, n1.AttachmentAncestry); err != nil {
		t.Fatal(err)
	}
	// The parent becomes known; the same delivery arrives with different content.
	r.insertPrompt(t, "px")
	att2 := []string{
		`{"type":"user","uuid":"px","message":{"role":"user","content":"parent"}}`,
		`{"type":"attachment","uuid":"att-x","parentUuid":"px","attachment":{"type":"file","filename":"/f","content":{"type":"text","file":{"filePath":"/f","content":"TWO different\n","numLines":1,"totalLines":1}}}}`,
	}
	n2 := normalizeBatch(t, att2)
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 2000, n2.Turns, n2.Contents, n2.CallOwners, n2.Ancestry, n2.AttachmentAncestry); err == nil {
		t.Fatal("changed content for the same delivery was accepted after ownership resolved")
	}
}

// A later parent resolves an attachment already retained in storage.
func TestObservedInput_RetainedUnresolvedReconciled(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)
	n1 := normalizeBatch(t, []string{
		`{"type":"attachment","uuid":"att-y","parentUuid":"py","attachment":{"type":"file","filename":"/f","content":{"type":"text","file":{"filePath":"/f","content":"body\n","numLines":1,"totalLines":1}}}}`,
	})
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, n1.Turns, n1.Contents, n1.CallOwners, n1.Ancestry, n1.AttachmentAncestry); err != nil {
		t.Fatal(err)
	}
	// Persisting the parent resolves the retained attachment.
	turnID := r.insertPrompt(t, "py")
	n2 := normalizeBatch(t, []string{`{"type":"user","uuid":"py","message":{"role":"user","content":"parent request"}}`})
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 2000, n2.Turns, n2.Contents, n2.CallOwners, n2.Ancestry, n2.AttachmentAncestry); err != nil {
		t.Fatal(err)
	}
	res, err := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, turnID)
	if err != nil || res.Evidence == nil || len(res.Evidence.Observations) != 1 {
		t.Fatalf("retained attachment not reconciled: %+v err=%v", res, err)
	}
	if res.Evidence.Observations[0].Scope != observedinput.ScopeRequestEnvelope {
		t.Fatalf("reconciled attachment not promoted to envelope: %+v", res.Evidence.Observations[0])
	}
}

// Attachments can reach their request through another attachment's parent link.
func TestObservedInput_ChainedAttachmentResolvesToRequest(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)

	batch := []string{
		`{"type":"user","uuid":"r1","message":{"role":"user","content":"request with nested attachments"}}`,
		`{"type":"attachment","uuid":"att-text","parentUuid":"r1","attachment":{"type":"file","filename":"/work/notes.txt","content":{"type":"text","file":{"filePath":"/work/notes.txt","content":"outer note\n","numLines":1,"totalLines":1}}}}`,
		`{"type":"attachment","uuid":"att-nested","parentUuid":"att-text","attachment":{"type":"file","filename":"/work/nested.txt","content":{"type":"text","file":{"filePath":"/work/nested.txt","content":"nested payload\n","numLines":1,"totalLines":1}}}}`,
	}

	t1 := r.insertPrompt(t, "r1")
	n := normalizeBatch(t, batch)
	// The nested attachment requires ancestry resolution during persistence.
	if u := findBatchTurn(n, "unresolved"); u == nil {
		t.Fatal("expected the nested attachment to normalize as unresolved")
	}
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
		t.Fatalf("persist: %v", err)
	}

	res, err := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, t1)
	if err != nil || res.Evidence == nil {
		t.Fatalf("collect: %+v err=%v", res, err)
	}
	if len(res.Evidence.Observations) != 2 {
		t.Fatalf("both attachments should resolve to the turn, got %d: %+v", len(res.Evidence.Observations), res.Evidence.Observations)
	}
	for _, o := range res.Evidence.Observations {
		if o.Scope != observedinput.ScopeRequestEnvelope {
			t.Fatalf("attachment %s not promoted to envelope scope: %+v", o.DeliveryID, o)
		}
	}
	// Both attachments belong to the same request.
	linkByDelivery := map[string]string{}
	for _, l := range res.Evidence.RequestLinks {
		linkByDelivery[l.DeliveryID] = l.RequestID
	}
	outer, nested := linkByDelivery["att:att-text"], linkByDelivery["att:att-nested"]
	if nested == "" {
		t.Fatal("nested attachment has no request membership link")
	}
	if nested != outer {
		t.Fatalf("nested attachment linked to %q, want the chain-root request %q", nested, outer)
	}
}

// Resolving retained ancestry preserves the attachment's delivery identity.
func TestObservedInput_ChainedAttachmentSplitBatchNoConflict(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)

	// Retain the nested attachment before its parent arrives.
	n1 := normalizeBatch(t, []string{
		`{"type":"attachment","uuid":"att-nested","parentUuid":"att-outer","attachment":{"type":"file","filename":"/work/nested.txt","content":{"type":"text","file":{"filePath":"/work/nested.txt","content":"nested payload\n","numLines":1,"totalLines":1}}}}`,
	})
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, n1.Turns, n1.Contents, n1.CallOwners, n1.Ancestry, n1.AttachmentAncestry); err != nil {
		t.Fatalf("persist batch1: %v", err)
	}

	// Supply the missing parent and request.
	t1 := r.insertPrompt(t, "r1")
	n2 := normalizeBatch(t, []string{
		`{"type":"user","uuid":"r1","message":{"role":"user","content":"request"}}`,
		`{"type":"attachment","uuid":"att-outer","parentUuid":"r1","attachment":{"type":"file","filename":"/work/outer.txt","content":{"type":"text","file":{"filePath":"/work/outer.txt","content":"outer\n","numLines":1,"totalLines":1}}}}`,
	})
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 2000, n2.Turns, n2.Contents, n2.CallOwners, n2.Ancestry, n2.AttachmentAncestry); err != nil {
		t.Fatalf("promote retained chained attachment: %v", err)
	}

	res, err := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, t1)
	if err != nil || res.Evidence == nil {
		t.Fatalf("collect: %+v err=%v", res, err)
	}
	if len(res.Evidence.Observations) != 2 {
		t.Fatalf("expected both attachments on the turn, got %d: %+v", len(res.Evidence.Observations), res.Evidence.Observations)
	}
	var nestedLink string
	for _, l := range res.Evidence.RequestLinks {
		if l.DeliveryID == "att:att-nested" {
			nestedLink = l.RequestID
		}
	}
	if nestedLink != "req:r1" {
		t.Fatalf("nested attachment linked to %q, want req:r1", nestedLink)
	}
}

// Attachments parented to tool results stay outside the request envelope.
func TestRetestRuntimeAttachmentIsNotRequestEnvelope(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)
	t1 := r.insertPrompt(t, "r1")

	batch := normalizeBatch(t, []string{
		`{"type":"user","uuid":"r1","message":{"role":"user","content":"do the task"}}`,
		`{"type":"attachment","uuid":"att-good","parentUuid":"r1","attachment":{"type":"file","filename":"spec.md","content":{"type":"text","file":{"filePath":"spec.md","content":"spec\n","numLines":1,"totalLines":1}}}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"r1","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read"}]}}`,
		`{"type":"user","uuid":"res1","parentUuid":"a1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"file body"}]}}`,
		`{"type":"attachment","uuid":"att-reminder","parentUuid":"res1","attachment":{"type":"file","filename":"reminder","content":{"type":"text","file":{"filePath":"reminder","content":"runtime reminder\n","numLines":1,"totalLines":1}}}}`,
	})
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, batch.Turns, batch.Contents, batch.CallOwners, batch.Ancestry, batch.AttachmentAncestry); err != nil {
		t.Fatalf("persist: %v", err)
	}

	res, err := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, t1)
	if err != nil || res.Evidence == nil {
		t.Fatalf("collect: %+v err=%v", res, err)
	}
	envelope := map[string]bool{}
	for _, o := range res.Evidence.Observations {
		if o.Scope == observedinput.ScopeRequestEnvelope {
			envelope[o.DeliveryID] = true
		}
	}
	if !envelope["att:att-good"] {
		t.Fatal("the genuine attachment on the request should be a request-envelope input")
	}
	if envelope["att:att-reminder"] {
		t.Fatal("a runtime attachment parented to a tool result must not become a request-envelope input")
	}
	for _, l := range res.Evidence.RequestLinks {
		if l.DeliveryID == "att:att-reminder" {
			t.Fatalf("runtime attachment gained a request membership link: %+v", l)
		}
	}
}

// Collection preserves top-level gaps.
func TestObservedInput_TopLevelGapsRetained(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)
	n := normalizeFixtureForRepo(t, "captured_pdf_attachment.jsonl")
	turnID := r.insertPrompt(t, requestUUID(n))
	ev := n.Turns[0]
	ev.Gaps = append(ev.Gaps, observedinput.Gap{Reason: observedinput.GapUnsupportedShape, Detail: "recorded limitation"})
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, []observedinput.Evidence{ev}, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
		t.Fatal(err)
	}
	res, err := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, turnID)
	if err != nil || res.Evidence == nil {
		t.Fatalf("collect: %+v err=%v", res, err)
	}
	if len(res.Evidence.Gaps) == 0 {
		t.Fatalf("top-level gap disappeared: %+v", res.Evidence)
	}
}

// A result in a later batch belongs to the turn that issued its call.
func TestObservedInput_SplitCallAndResult(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)
	turnID := r.insertPrompt(t, "r1")

	// Batch 1: prompt and the tool call.
	n1 := normalizeBatch(t, []string{
		`{"type":"user","uuid":"r1","message":{"role":"user","content":"do it"}}`,
		`{"type":"assistant","uuid":"a1","parentUuid":"r1","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read"}]}}`,
	})
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, n1.Turns, n1.Contents, n1.CallOwners, n1.Ancestry, n1.AttachmentAncestry); err != nil {
		t.Fatal(err)
	}
	// Normalize the result without the earlier call record.
	n2 := normalizeBatch(t, []string{
		`{"type":"user","uuid":"res","parentUuid":"a1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"output"}]}}`,
	})
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 2000, n2.Turns, n2.Contents, n2.CallOwners, n2.Ancestry, n2.AttachmentAncestry); err != nil {
		t.Fatal(err)
	}
	res, err := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, turnID)
	if err != nil || res.Evidence == nil {
		t.Fatalf("collect: %+v err=%v", res, err)
	}
	var found bool
	for _, o := range res.Evidence.Observations {
		if o.Acquisition == observedinput.AcquisitionToolResult && o.Scope == observedinput.ScopeObservedContext {
			found = true
		}
	}
	if !found {
		t.Fatalf("cross-batch tool result not attributed to its turn: %+v", res.Evidence.Observations)
	}
}

// Rejected deliveries must not appear in collected evidence.
func TestObservedInput_RejectedConflictNotPublished(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)
	n := normalizeBatch(t, []string{
		`{"type":"attachment","uuid":"att-z","parentUuid":"pz","attachment":{"type":"file","filename":"/f","content":{"type":"text","file":{"filePath":"/f","content":"one\n","numLines":1,"totalLines":1}}}}`,
	})
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
		t.Fatal(err)
	}
	turnID := r.insertPrompt(t, "pz")
	changed := normalizeBatch(t, []string{
		`{"type":"user","uuid":"pz","message":{"role":"user","content":"parent"}}`,
		`{"type":"attachment","uuid":"att-z","parentUuid":"pz","attachment":{"type":"file","filename":"/f","content":{"type":"text","file":{"filePath":"/f","content":"CHANGED\n","numLines":1,"totalLines":1}}}}`,
	})
	_ = PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 2000, changed.Turns, changed.Contents, changed.CallOwners, changed.Ancestry, changed.AttachmentAncestry)
	// The changed delivery's item must not be published under the turn.
	res, _ := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, turnID)
	if res.Evidence != nil {
		for _, o := range res.Evidence.Observations {
			if o.Representation.ContentRef != "" {
				b, _ := blobs.NewStore(filepath.Join(r.path, ".semantica", "objects"))
				raw, _ := b.Get(ctx, o.Representation.ContentRef)
				if strings.Contains(string(raw), "CHANGED") {
					t.Fatal("rejected conflicting delivery was published")
				}
			}
		}
	}
}

// A delivery's provider parent cannot change, even when its content is unchanged.
func TestObservedInput_StructuralIdentityImmutable(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)
	r.insertPrompt(t, "pa")
	r.insertPrompt(t, "pb")
	// Replay the same delivery and content with a different parent.
	body := `"content":{"type":"text","file":{"filePath":"/f","content":"same\n","numLines":1,"totalLines":1}}`
	b1 := normalizeBatch(t, []string{
		`{"type":"user","uuid":"pa","message":{"role":"user","content":"A"}}`,
		`{"type":"attachment","uuid":"dup","parentUuid":"pa","attachment":{"type":"file","filename":"/f",` + body + `}}`,
	})
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, b1.Turns, b1.Contents, b1.CallOwners, b1.Ancestry, b1.AttachmentAncestry); err != nil {
		t.Fatal(err)
	}
	b2 := normalizeBatch(t, []string{
		`{"type":"user","uuid":"pb","message":{"role":"user","content":"B"}}`,
		`{"type":"attachment","uuid":"dup","parentUuid":"pb","attachment":{"type":"file","filename":"/f",` + body + `}}`,
	})
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 2000, b2.Turns, b2.Contents, b2.CallOwners, b2.Ancestry, b2.AttachmentAncestry); err == nil {
		t.Fatal("same delivery id acquired a different parent without conflict")
	}
}

// Gaps from separate batches accumulate under a turn.
func TestObservedInput_GapsAccumulate(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)
	n := normalizeFixtureForRepo(t, "captured_pdf_attachment.jsonl")
	turnID := r.insertPrompt(t, requestUUID(n))
	ev := n.Turns[0]
	ev1 := ev
	ev1.Gaps = []observedinput.Gap{{Reason: observedinput.GapUnsupportedShape, Detail: "first"}}
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, []observedinput.Evidence{ev1}, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
		t.Fatal(err)
	}
	ev2 := ev
	ev2.Gaps = []observedinput.Gap{{Reason: observedinput.GapSizeLimit, Detail: "second"}}
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 2000, []observedinput.Evidence{ev2}, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
		t.Fatalf("second gap batch conflicted: %v", err)
	}
	res, _ := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, turnID)
	if res.Evidence == nil || len(res.Evidence.Gaps) != 2 {
		t.Fatalf("gaps did not accumulate: %+v", res.Evidence)
	}
}

// Request, call, and result records retain ownership across separate batches.
func TestObservedInput_ThreeBatchCallAncestry(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)
	turnID := r.insertPrompt(t, "r1")
	b1 := normalizeBatch(t, []string{`{"type":"user","uuid":"r1","message":{"role":"user","content":"go"}}`})
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, b1.Turns, b1.Contents, b1.CallOwners, b1.Ancestry, b1.AttachmentAncestry); err != nil {
		t.Fatal(err)
	}
	// The call refers to a request from the previous batch.
	b2 := normalizeBatch(t, []string{`{"type":"assistant","uuid":"a1","parentUuid":"r1","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read"}]}}`})
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 2000, b2.Turns, b2.Contents, b2.CallOwners, b2.Ancestry, b2.AttachmentAncestry); err != nil {
		t.Fatal(err)
	}
	// Batch 3: the result.
	b3 := normalizeBatch(t, []string{`{"type":"user","uuid":"res","parentUuid":"a1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"out"}]}}`})
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 3000, b3.Turns, b3.Contents, b3.CallOwners, b3.Ancestry, b3.AttachmentAncestry); err != nil {
		t.Fatal(err)
	}
	res, _ := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, turnID)
	found := false
	if res.Evidence != nil {
		for _, o := range res.Evidence.Observations {
			if o.Acquisition == observedinput.AcquisitionToolResult {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("result lost ownership across [request][call][result]: %+v", res.Evidence)
	}
}

func TestObservedInput_FourBatchDeepAncestry(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)
	turnID := r.insertPrompt(t, "r1")
	persist := func(recs []string, at int64) {
		n := normalizeBatch(t, recs)
		if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, at, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
			t.Fatal(err)
		}
	}
	persist([]string{`{"type":"user","uuid":"r1","message":{"role":"user","content":"go"}}`}, 1000)
	persist([]string{`{"type":"assistant","uuid":"a1","parentUuid":"r1","message":{"role":"assistant","content":[{"type":"text","text":"thinking"}]}}`}, 2000)
	persist([]string{`{"type":"assistant","uuid":"c1","parentUuid":"a1","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read"}]}}`}, 3000)
	persist([]string{`{"type":"user","uuid":"res","parentUuid":"c1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"out"}]}}`}, 4000)

	res, _ := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, turnID)
	found := false
	if res.Evidence != nil {
		for _, o := range res.Evidence.Observations {
			if o.Acquisition == observedinput.AcquisitionToolResult && o.Scope == observedinput.ScopeObservedContext {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("result lost ownership through a deep ancestry chain: %+v", res.Evidence)
	}
}

// insertLineageEvent stores a non-request row with an assigned turn.
func (r observedRepo) insertLineageEvent(t *testing.T, providerEventID, turnID, kind string) {
	t.Helper()
	ctx := context.Background()
	h, err := sqlstore.Open(ctx, filepath.Join(r.path, ".semantica", "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: uuid.NewString(), SessionID: r.sessionID, RepositoryID: r.repoID, Ts: 1, Kind: kind,
		Role: sql.NullString{String: "user", Valid: true}, ProviderEventID: sql.NullString{String: providerEventID, Valid: true},
		TurnID: sql.NullString{String: turnID, Valid: true}, EventSource: "transcript",
	}); err != nil {
		t.Fatal(err)
	}
}

// Non-request turn assignments must not override request ancestry.
func TestObservedInput_NonRequestAncestorDoesNotAnchor(t *testing.T) {
	ctx := context.Background()
	r := newObservedRepo(t)
	t1 := r.insertPrompt(t, "r1")
	// Lineage assigned the delayed result to a different turn.
	r.insertLineageEvent(t, "dr", "some-other-turn", "tool_result")

	// Ancestry: new call -> delayed result -> request; the new call issues toolu_2.
	n := normalizeBatch(t, []string{
		`{"uuid":"dr","parentUuid":"r1"}`,
		`{"type":"assistant","uuid":"c2","parentUuid":"dr","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_2","name":"Read"}]}}`,
		`{"type":"user","uuid":"res2","parentUuid":"c2","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"out"}]}}`,
	})
	if err := PersistObservedInputs(ctx, r.path, "claude_code", r.providerSession, 1000, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
		t.Fatal(err)
	}
	// The request determines the owning turn.
	res, _ := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, t1)
	found := false
	if res.Evidence != nil {
		for _, o := range res.Evidence.Observations {
			if o.Acquisition == observedinput.AcquisitionToolResult {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("ownership did not reach the actual request through a non-request ancestor: %+v", res.Evidence)
	}
	other, _ := CollectObservedInput(ctx, r.path, "claude_code", r.sessionID, "some-other-turn")
	if other.Evidence != nil {
		t.Fatalf("ownership terminated at the non-request ancestor's turn: %+v", other.Evidence)
	}
}

// Propagation copies retained evidence and its referenced content to the destination.
func TestObservedInput_CrossRepoPropagation(t *testing.T) {
	ctx := context.Background()
	// Launch repo A holds the request and the retained evidence.
	a := newObservedRepo(t)
	n := normalizeFixtureForRepo(t, "captured_pdf_attachment.jsonl")
	turnID := a.insertPrompt(t, requestUUID(n))
	if err := PersistObservedInputs(ctx, a.path, "claude_code", a.providerSession, 1000, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
		t.Fatalf("persist in launch repo: %v", err)
	}

	// Destination repo B shares the provider session but has no local evidence.
	b := newObservedRepo(t)
	if empty, _ := CollectObservedInput(ctx, b.path, "claude_code", b.sessionID, turnID); empty.Evidence != nil {
		t.Fatal("destination unexpectedly already had evidence")
	}

	bBlobs, err := blobs.NewStore(repoObjects(b.path))
	if err != nil {
		t.Fatal(err)
	}
	res, err := PropagateObservedInput(ctx, bBlobs, a.path, "claude_code", a.providerSession, b.sessionID, turnID)
	if err != nil || res.Evidence == nil {
		t.Fatalf("propagate: %+v err=%v", res, err)
	}
	if res.Evidence.SessionID != b.sessionID || res.Evidence.TurnID != turnID {
		t.Fatalf("mirrored evidence not re-keyed to destination: %+v", res.Evidence)
	}
	if len(res.Evidence.Requests) != len(n.Turns[0].Requests) && len(res.Evidence.Requests) == 0 {
		t.Fatalf("mirrored evidence lost its requests: %+v", res.Evidence)
	}
	// The content closure must resolve in the destination store.
	if !closureResolves(ctx, bBlobs, *res.Evidence) {
		t.Fatal("mirrored evidence closure does not resolve in the destination repo")
	}
	if err := observedinput.Validate(*res.Evidence); err != nil {
		t.Fatalf("mirrored evidence invalid: %v", err)
	}
}

// Packaging uses the recorded origin despite a nearer database on disk.
func TestObservedInput_CrossRepoPackageUsesRecordedRoot(t *testing.T) {
	ctx := context.Background()
	a := newObservedRepo(t)
	n := normalizeFixtureForRepo(t, "captured_pdf_attachment.jsonl")
	t1 := a.insertPrompt(t, requestUUID(n))
	if err := PersistObservedInputs(ctx, a.path, "claude_code", a.providerSession, 1000, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
		t.Fatalf("persist in launch repo: %v", err)
	}

	// The nested database must not override the recorded origin.
	nestedSem := filepath.Join(a.path, "nested", ".semantica")
	if err := os.MkdirAll(nestedSem, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sqlstore.MigratePath(ctx, filepath.Join(nestedSem, "lineage.db")); err != nil {
		t.Fatal(err)
	}

	// Destination B records A as its origin.
	b := newDestRepo(t, a.path)
	bs, err := blobs.NewStore(filepath.Join(b.path, ".semantica", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	PackageTurn(ctx, b.path, TurnContext{Provider: "claude_code", SessionID: b.providerSession, TurnID: t1, StartedAt: 1, CompletedAt: 3000}, bs)

	var bundle struct {
		ObservedInput *bundleObservedInput `json:"observed_input"`
	}
	if err := json.Unmarshal(packagedBundle(t, b, t1, bs), &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.ObservedInput == nil || bundle.ObservedInput.Unavailable || bundle.ObservedInput.EvidenceHash == "" {
		t.Fatalf("packaging dropped mirrored observed_input from recorded root: %+v", bundle.ObservedInput)
	}
	if _, err := bs.Get(ctx, bundle.ObservedInput.EvidenceHash); err != nil {
		t.Fatalf("mirrored evidence document unresolved in destination: %v", err)
	}
}

// A deleted origin is unavailable and must not be recreated.
func TestObservedInput_DeletedOriginIsUnavailableAndNotRecreated(t *testing.T) {
	ctx := context.Background()
	a := newObservedRepo(t)
	n := normalizeFixtureForRepo(t, "captured_pdf_attachment.jsonl")
	t1 := a.insertPrompt(t, requestUUID(n))
	if err := PersistObservedInputs(ctx, a.path, "claude_code", a.providerSession, 1000, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
		t.Fatalf("persist in launch repo: %v", err)
	}
	b := newDestRepo(t, a.path)

	dbPath := filepath.Join(a.path, ".semantica", "lineage.db")
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}

	bs, err := blobs.NewStore(filepath.Join(b.path, ".semantica", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	PackageTurn(ctx, b.path, TurnContext{Provider: "claude_code", SessionID: b.providerSession, TurnID: t1, StartedAt: 1, CompletedAt: 3000}, bs)

	var bundle struct {
		ObservedInput *bundleObservedInput `json:"observed_input"`
	}
	if err := json.Unmarshal(packagedBundle(t, b, t1, bs), &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.ObservedInput == nil || !bundle.ObservedInput.Unavailable {
		t.Fatalf("deleted origin must surface as unavailable, got %+v", bundle.ObservedInput)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("origin database was recreated during packaging: %v", err)
	}
}

// A missing session in a valid origin means no evidence was recorded there.
func TestObservedInput_MissingSessionInValidOriginIsAbsent(t *testing.T) {
	ctx := context.Background()
	a := newObservedRepo(t) // valid origin DB and repository row, but no matching session
	b := newDestRepo(t, a.path)
	bs, err := blobs.NewStore(filepath.Join(b.path, ".semantica", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := PropagateObservedInput(ctx, bs, a.path, "claude_code", "no-such-provider-session", b.sessionID, "some-turn")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Unavailable {
		t.Fatal("missing session in a valid origin must be absence, not unavailable")
	}
	if res.Evidence != nil {
		t.Fatalf("expected no evidence for an absent session, got %+v", res.Evidence)
	}
}

// Origin storage failures produce unavailable evidence.
func TestObservedInput_CrossRepoOriginFailureIsUnavailable(t *testing.T) {
	ctx := context.Background()
	a := newObservedRepo(t)
	n := normalizeFixtureForRepo(t, "captured_pdf_attachment.jsonl")
	t1 := a.insertPrompt(t, requestUUID(n))
	if err := PersistObservedInputs(ctx, a.path, "claude_code", a.providerSession, 1000, n.Turns, n.Contents, n.CallOwners, n.Ancestry, n.AttachmentAncestry); err != nil {
		t.Fatalf("persist in launch repo: %v", err)
	}
	b := newDestRepo(t, a.path)

	// Corrupt the origin after retention to test lookup failure.
	if err := os.WriteFile(filepath.Join(a.path, ".semantica", "lineage.db"), []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}

	bs, err := blobs.NewStore(filepath.Join(b.path, ".semantica", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	PackageTurn(ctx, b.path, TurnContext{Provider: "claude_code", SessionID: b.providerSession, TurnID: t1, StartedAt: 1, CompletedAt: 3000}, bs)

	var bundle struct {
		ObservedInput *bundleObservedInput `json:"observed_input"`
	}
	if err := json.Unmarshal(packagedBundle(t, b, t1, bs), &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.ObservedInput == nil || !bundle.ObservedInput.Unavailable {
		t.Fatalf("origin failure must surface as unavailable, got %+v", bundle.ObservedInput)
	}
}

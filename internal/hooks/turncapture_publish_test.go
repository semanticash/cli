package hooks

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/provenance"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/turncapture"
)

func TestTurnObservationPublishesContextWithoutAuthorship(t *testing.T) {
	for _, provider := range []string{"cursor", "claude-code", "kiro-cli", "gemini-cli"} {
		t.Run(provider, func(t *testing.T) { testTurnObservationContext(t, provider) })
	}
}

type publicationProvider struct {
	*fakeProvider
	lineageSession string
}

func (p *publicationProvider) DeriveProviderSessionID(string) string { return p.lineageSession }

func testTurnObservationContext(t *testing.T, provider string) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	a := newToolWindowWorld(t, home, "A")
	defer func() { _ = broker.Close(a.bh) }()
	b := newToolWindowWorldAt(t, a.bh, filepath.Join(t.TempDir(), "B"))
	c := newToolWindowWorldAt(t, a.bh, filepath.Join(t.TempDir(), "C"))
	p := &publicationProvider{fakeProvider: &fakeProvider{name: provider}}
	lineageSession := "workspace-session"
	if provider == "gemini-cli" {
		lineageSession = "transcript-session"
		p.lineageSession = lineageSession
	}
	prompt := &Event{Type: PromptSubmitted, SessionID: "workspace-session", ProviderSessionID: "native-session", ProviderTurnID: "generation", Prompt: "Change B", CWD: a.repoPath, Timestamp: time.Now().UnixMilli()}
	if err := Dispatch(ctx, p, prompt, a.bh, nil); err != nil {
		t.Fatal(err)
	}
	state, err := LoadCaptureState(prompt.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	source, err := blobs.NewStore(filepath.Join(home, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	promptHash, _, err := source.Put(ctx, []byte(`{"type":"user","message":{"content":"Change B"}}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = broker.WriteEventsToRepo(ctx, a.repoPath, []broker.RawEvent{{
		EventID: "prompt", SourceKey: "source", Provider: provider, ProviderSessionID: lineageSession,
		TurnID: state.TurnID, Kind: "user", Role: "user", PayloadHash: promptHash, EventSource: "hook",
		Timestamp: prompt.Timestamp, SourceProjectPath: a.repoPath,
	}}, source)
	if err != nil {
		t.Fatal(err)
	}
	sourceEvents := 1
	if provider == "claude-code" {
		hash, _, err := source.Put(ctx, []byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Changed B."}]}}`))
		if err != nil {
			t.Fatal(err)
		}
		_, err = broker.WriteEventsToRepo(ctx, a.repoPath, []broker.RawEvent{{
			EventID: "response", SourceKey: "source", Provider: provider, ProviderSessionID: prompt.SessionID,
			TurnID: state.TurnID, Kind: "assistant", Role: "assistant", PayloadHash: hash, EventSource: "transcript",
			Timestamp: prompt.Timestamp + 1, SourceProjectPath: a.repoPath,
		}}, source)
		if err != nil {
			t.Fatal(err)
		}
		sourceEvents++
	}
	if provider == "kiro-cli" || provider == "gemini-cli" {
		response := provenance.RedactAndStoreResponse(ctx, source, "response", "Changed B.", prompt.Timestamp+1)
		state.ResponseStatus, state.ResponseHash = response.Status, response.Hash
		state.ResponseCompletedAt = response.CompletedAt
		if err := SaveCaptureState(state); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(b.repoPath, "inner.txt"), []byte("committed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, b.repoPath, "-c", "core.hooksPath="+home, "commit", "-am", "in turn")
	if err := os.WriteFile(filepath.Join(b.repoPath, "inner.txt"), []byte("after commit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Concurrent human edits must remain context-only.
	if err := os.WriteFile(filepath.Join(c.repoPath, "inner.txt"), []byte("human edit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := &Event{Type: AgentCompleted, SessionID: prompt.SessionID, ProviderSessionID: prompt.ProviderSessionID, ProviderTurnID: prompt.ProviderTurnID, CWD: a.repoPath, Timestamp: time.Now().UnixMilli()}
	if err := Dispatch(ctx, p, stop, a.bh, source); err != nil {
		t.Fatal(err)
	}
	for _, w := range []*toolWindowWorld{b, c} {
		if found, err := provenance.TurnRecorded(ctx, w.repoPath, provider, lineageSession, state.TurnID); err != nil || !found {
			t.Fatalf("packaging cannot discover destination turn: %v", err)
		}
		h, err := sqlstore.Open(ctx, filepath.Join(w.semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = sqlstore.Close(h) }()
		var hash, kind, role, routing string
		var count int
		if err := h.DB.QueryRow(`select l.evidence_hash, e.kind, e.role, e.tool_uses from agent_event_evidence_links l join agent_events e on e.event_id=l.event_id where l.evidence_kind='turn_observation'`).Scan(&hash, &kind, &role, &routing); err != nil {
			t.Fatal(err)
		}
		if kind != "context" || role != "system" || !strings.Contains(routing, "context_only") {
			t.Fatalf("observation claims authorship: %s %s %s", kind, role, routing)
		}
		bs, err := blobs.NewStore(filepath.Join(w.semDir, "objects"))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := bs.Get(ctx, hash)
		if err != nil {
			t.Fatal(err)
		}
		var projected turncapture.Record
		if err := json.Unmarshal(raw, &projected); err != nil {
			t.Fatal(err)
		}
		if len(projected.Repositories) != 1 || projected.Repositories[0].Subject.RepositoryID != w.repoID || projected.End.TrackedCompletion != "unknown" {
			t.Fatalf("projection lost identity or uncertainty: %+v", projected)
		}
		if provider == "kiro-cli" && projected.SessionID != prompt.ProviderSessionID {
			t.Fatal("recorder lost native session identity")
		}
		if w == b && len(projected.End.Repositories[0].Changes) != 2 {
			t.Fatal("commit or remaining dirty change missing")
		}
		if len(projected.Evidence) != 0 || len(projected.End.Repositories) != 1 {
			t.Fatal("projection contains mutable or unrelated evidence")
		}
		links, err := h.Queries.ListEvidenceLinksInWindow(ctx, sqldb.ListEvidenceLinksInWindowParams{RepositoryID: w.repoID, AfterTs: 0, UpToTs: time.Now().Add(time.Hour).UnixMilli()})
		if err != nil || len(links) != 0 {
			t.Fatalf("turn observation entered tool attribution: %+v %v", links, err)
		}
		var bundleHash string
		if err := h.DB.QueryRow(`select provenance_bundle_hash from provenance_manifests where turn_id=? and kind='turn_bundle'`, state.TurnID).Scan(&bundleHash); err != nil {
			t.Fatal(err)
		}
		bundle, err := bs.Get(ctx, bundleHash)
		if err != nil || !strings.Contains(string(bundle), promptHash) {
			t.Fatalf("destination lacks original prompt: %s %v", bundle, err)
		}
		var packaged struct {
			Association struct {
				Basis      string `json:"basis"`
				Authorship string `json:"authorship"`
			} `json:"repository_association"`
		}
		if err := json.Unmarshal(bundle, &packaged); err != nil {
			t.Fatal(err)
		}
		if packaged.Association.Basis != "turn_observation" || packaged.Association.Authorship != "unknown" {
			t.Error("bundle lost observation-only association")
		}
		if provider != "cursor" {
			var status, responseHash string
			if err := h.DB.QueryRow(`select response_status, coalesce(response_hash, '') from provenance_manifests where turn_id=? and kind='turn_bundle'`, state.TurnID).Scan(&status, &responseHash); err != nil {
				t.Fatal(err)
			}
			if status != "complete" || responseHash == "" {
				t.Fatalf("destination lost source response: %s %s", status, responseHash)
			}
			body, err := bs.Get(ctx, responseHash)
			if err != nil || !strings.Contains(string(body), "Changed B.") {
				t.Fatalf("response object unavailable: %v", err)
			}
		}
		if err := publishTurnObservation(ctx, p, stop, state, a.bh); err != nil {
			t.Fatal(err)
		}
		wantLinks := 1
		for _, change := range projected.End.Repositories[0].Changes {
			if change.Commit == "" || len(change.Files) == 0 {
				continue
			}
			wantLinks++
			var linkedHash string
			if err := h.DB.QueryRow(`select evidence_hash from agent_event_evidence_links where evidence_kind='turn_observation' and group_id=?`, change.Commit).Scan(&linkedHash); err != nil || linkedHash != hash {
				t.Fatalf("commit lost its observation: %s %v", linkedHash, err)
			}
		}
		if err := h.DB.QueryRow(`select count(*) from agent_event_evidence_links where evidence_kind='turn_observation'`).Scan(&count); err != nil || count != wantLinks {
			t.Fatalf("retry changed observation links: %d, want %d: %v", count, wantLinks, err)
		}
		if err := h.DB.QueryRow(`select count(distinct event_id) from agent_event_evidence_links where evidence_kind='turn_observation'`).Scan(&count); err != nil || count != 1 {
			t.Fatalf("retry duplicated observation event: %d %v", count, err)
		}
	}
	if agentEventCount(t, a.repoPath) != sourceEvents {
		t.Fatal("unchanged source received observation event")
	}
	wrong := *state
	wrong.TurnID = "another-turn"
	if err := publishTurnObservation(ctx, p, stop, &wrong, a.bh); err == nil {
		t.Fatal("accepted mismatched turn identity")
	}
}

// Direct observation publication records the resolved launch root.
func TestTurnObservationRecordsResolvedLaunchRoot(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	a := newToolWindowWorld(t, home, "A")
	defer func() { _ = broker.Close(a.bh) }()
	b := newToolWindowWorldAt(t, a.bh, filepath.Join(t.TempDir(), "B"))

	// Launch the session from a subdirectory inside A.
	subdir := filepath.Join(a.repoPath, "sub")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	p := &fakeProvider{name: "cursor"}
	prompt := &Event{Type: PromptSubmitted, SessionID: "s", ProviderTurnID: "g", CWD: subdir, Timestamp: time.Now().UnixMilli()}
	if err := Dispatch(ctx, p, prompt, a.bh, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.repoPath, "inner.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := &Event{Type: AgentCompleted, SessionID: "s", ProviderTurnID: "g", CWD: subdir, Timestamp: time.Now().UnixMilli()}
	if err := Dispatch(ctx, p, stop, a.bh, nil); err != nil {
		t.Fatal(err)
	}

	h, err := sqlstore.Open(ctx, filepath.Join(b.semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	var srp string
	if err := h.DB.QueryRow(`select coalesce(source_repo_path,'') from agent_sessions limit 1`).Scan(&srp); err != nil {
		t.Fatal(err)
	}
	if srp != a.repoPath {
		t.Fatalf("observation recorded origin %q, want resolved launch root %q", srp, a.repoPath)
	}
}

func TestTurnObservationPublicationFailureRetainsRetryState(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	a := newToolWindowWorld(t, home, "A")
	defer func() { _ = broker.Close(a.bh) }()
	b := newToolWindowWorldAt(t, a.bh, filepath.Join(t.TempDir(), "B"))
	p := &fakeProvider{name: "cursor"}
	prompt := &Event{Type: PromptSubmitted, SessionID: "s", ProviderTurnID: "g", CWD: a.repoPath, Timestamp: time.Now().UnixMilli()}
	if err := Dispatch(ctx, p, prompt, a.bh, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.repoPath, "inner.txt"), []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	objects := filepath.Join(b.semDir, "objects")
	if err := os.WriteFile(objects, []byte("block store creation"), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := &Event{Type: AgentCompleted, SessionID: "s", ProviderTurnID: "g", CWD: a.repoPath, Timestamp: time.Now().UnixMilli()}
	if err := Dispatch(ctx, p, stop, a.bh, nil); err == nil {
		t.Fatal("publication failure was ignored")
	}
	state, err := LoadCaptureState("s")
	if err != nil || state.TurnObservationKey == "" {
		t.Fatalf("publication failure discarded retry identity: %+v %v", state, err)
	}
	before, err := json.Marshal(observationRecords(t, home)[0].End)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(objects); err != nil {
		t.Fatal(err)
	}
	if err := Dispatch(ctx, p, stop, a.bh, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(observationRecords(t, home)[0].End)
	if string(before) != string(after) {
		t.Fatal("publication retry changed frozen observation")
	}
	if agentEventCount(t, b.repoPath) != 1 {
		t.Fatal("publication retry lost or duplicated context")
	}
}

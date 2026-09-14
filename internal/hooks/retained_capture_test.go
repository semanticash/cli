package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/agents/api"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
)

func TestDurableCapture_AdvancesOffsetBeforeDeliveryAndRecovers(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	t.Setenv("SEMANTICA_DURABLE_ROUTING", "1")
	w := newToolWindowWorld(t, home, "repo")
	src, err := blobs.NewStore(filepath.Join(home, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	hash, _, err := src.Put(ctx, []byte("retained transcript"))
	if err != nil {
		t.Fatal(err)
	}
	state := &CaptureState{SessionID: "retained", Provider: "test", TranscriptRef: "transcript", TranscriptOffset: 7, Timestamp: 1, TurnID: "turn"}
	if err := SaveCaptureState(state); err != nil {
		t.Fatal(err)
	}
	ev := broker.RawEvent{EventID: "durable-hook-test", Provider: "test", ProviderSessionID: "retained", SourceKey: "transcript", Kind: "assistant", Timestamp: 2, PayloadHash: hash, FilePaths: []string{filepath.Join(w.repoPath, "a.txt")}}
	provider := &fakeProvider{name: "test", readSequence: []fakeReadResult{{events: []broker.RawEvent{ev}, offset: 99}}}
	dbPath := filepath.Join(w.semDir, "lineage.db")
	if err := os.Rename(dbPath, dbPath+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := CaptureAndRoute(ctx, provider, &Event{SessionID: "retained"}, w.bh, src); err != nil {
		t.Fatal(err)
	}
	saved, err := LoadCaptureState("retained")
	if err != nil || saved.TranscriptOffset != 99 {
		t.Fatalf("offset not advanced after retention: %+v %v", saved, err)
	}
	pending, err := filepath.Glob(filepath.Join(home, "routing", "pending", "*.json"))
	if err != nil || len(pending) != 1 {
		t.Fatalf("missing retained event: %v %v", pending, err)
	}
	if err := os.Rename(dbPath+".saved", dbPath); err != nil {
		t.Fatal(err)
	}
	// Recovery does not need the source transcript, capture state, or current gate.
	t.Setenv("SEMANTICA_DURABLE_ROUTING", "0")
	if err := DeleteCaptureState("retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(home, "objects")); err != nil {
		t.Fatal(err)
	}
	if err := broker.DrainRetained(ctx, ""); err != nil {
		t.Fatal(err)
	}
	h, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	var turn, payload string
	if err := h.DB.QueryRowContext(ctx, `SELECT turn_id, payload_hash FROM agent_events WHERE event_id = ?`, ev.EventID).Scan(&turn, &payload); err != nil {
		t.Fatal(err)
	}
	if turn != "turn" || payload != hash {
		t.Fatalf("lost context/evidence: %s %s", turn, payload)
	}
}

func TestDurableCapture_RetentionFailureDoesNotAdvanceOffset(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	t.Setenv("SEMANTICA_DURABLE_ROUTING", "1")
	w := newToolWindowWorld(t, home, "repo")
	if err := SaveCaptureState(&CaptureState{SessionID: "retained", Provider: "test", TranscriptRef: "transcript", TranscriptOffset: 7, Timestamp: 1}); err != nil {
		t.Fatal(err)
	}
	ev := broker.RawEvent{EventID: "missing-blob", Provider: "test", Kind: "assistant", PayloadHash: strings.Repeat("a", 64), FilePaths: []string{filepath.Join(w.repoPath, "a.txt")}}
	provider := &fakeProvider{name: "test", readSequence: []fakeReadResult{{events: []broker.RawEvent{ev}, offset: 99}}}
	if err := CaptureAndRoute(ctx, provider, &Event{SessionID: "retained"}, w.bh, nil); err == nil {
		t.Fatal("retention failure accepted")
	}
	saved, err := LoadCaptureState("retained")
	if err != nil || saved.TranscriptOffset != 7 {
		t.Fatalf("offset moved without evidence: %+v %v", saved, err)
	}
}

func TestDurableCapture_ScopedCaptureRetainsOtherDestinations(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	t.Setenv("SEMANTICA_DURABLE_ROUTING", "1")
	w := newToolWindowWorld(t, home, "repo")
	other, _, _ := addBrokerRepo(t, w.bh, filepath.Dir(w.repoPath), "other")
	if err := SaveCaptureState(&CaptureState{SessionID: "retained", Provider: "test", TranscriptRef: "transcript", TranscriptOffset: 7, Timestamp: 1}); err != nil {
		t.Fatal(err)
	}
	ev := broker.RawEvent{EventID: "other-destination", Provider: "test", Kind: "assistant", FilePaths: []string{filepath.Join(other, "a.txt")}}
	provider := &fakeProvider{name: "test", readSequence: []fakeReadResult{{events: []broker.RawEvent{ev}, offset: 99}}}
	ok, err := CaptureAndRouteForRepo(ctx, provider, &Event{SessionID: "retained"}, w.bh, nil, w.repoPath)
	if err != nil || !ok {
		t.Fatalf("scoped capture: %v %v", ok, err)
	}
	saved, _ := LoadCaptureState("retained")
	if saved.TranscriptOffset != 99 {
		t.Fatal("cross-repository retention blocked the offset")
	}
	pending, _ := filepath.Glob(filepath.Join(home, "routing", "pending", "*.json"))
	if len(pending) != 1 {
		t.Fatal("other destination was not retained")
	}
	if err := broker.DrainRetained(ctx, ""); err != nil {
		t.Fatal(err)
	}
	pending, _ = filepath.Glob(filepath.Join(home, "routing", "pending", "*.json"))
	if len(pending) != 0 {
		t.Fatal("worker did not deliver other destination")
	}
}

// failingPayloadProvider mimics builders that omit a hash after a failed Put.
type failingPayloadProvider struct{ fakeProvider }

func (p *failingPayloadProvider) ReadFromOffset(ctx context.Context, _ string, _ int, bs api.BlobPutter) ([]broker.RawEvent, int, error) {
	_, _, _ = bs.Put(ctx, []byte("must not disappear"))
	return []broker.RawEvent{{EventID: "lost-payload", Provider: "test", Kind: "assistant"}}, 99, nil
}

func TestDurableCapture_RejectsSwallowedProviderBlobError(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	t.Setenv("SEMANTICA_DURABLE_ROUTING", "1")
	w := newToolWindowWorld(t, home, "repo")
	if err := SaveCaptureState(&CaptureState{SessionID: "retained", Provider: "test", TranscriptOffset: 7}); err != nil {
		t.Fatal(err)
	}
	provider := &failingPayloadProvider{fakeProvider: fakeProvider{name: "test"}}
	if err := CaptureAndRoute(ctx, provider, &Event{SessionID: "retained"}, w.bh, nil); err == nil {
		t.Fatal("swallowed payload failure was accepted")
	}
	state, err := LoadCaptureState("retained")
	if err != nil || state.TranscriptOffset != 7 {
		t.Fatalf("offset advanced: %+v %v", state, err)
	}
}

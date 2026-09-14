package broker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
)

func retainedFixture(t *testing.T) (context.Context, RawEvent, []RepoMatch, *blobs.Store, string) {
	t.Helper()
	ctx := context.Background()
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	root := t.TempDir()
	src, err := blobs.NewStore(filepath.Join(root, "source"))
	if err != nil {
		t.Fatal(err)
	}
	hash, _, err := src.Put(ctx, []byte(`{"message":"captured evidence"}`))
	if err != nil {
		t.Fatal(err)
	}
	ev := RawEvent{EventID: "retained-event", Provider: "test", SourceKey: "source", ProviderSessionID: "session", Kind: "assistant", Role: "assistant", Timestamp: 12, PayloadHash: hash, TurnID: "turn", TokenUsageValid: true}
	var matches []RepoMatch
	for _, name := range []string{"a", "b"} {
		path := tempRepoWithDB(t, filepath.Join(root, name))
		matches = append(matches, RepoMatch{Repo: RegisteredRepo{Path: path, CanonicalPath: CanonicalRepoPath(path), RepoID: name, Active: true}, Events: []RawEvent{ev}})
	}
	return ctx, ev, matches, src, root
}

func requireDeliveredEvent(t *testing.T, ctx context.Context, path string, ev RawEvent) {
	t.Helper()
	h, err := sqlstore.Open(ctx, filepath.Join(path, ".semantica", "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	var count int
	var hash, turn string
	var tokens int64
	if err := h.DB.QueryRowContext(ctx, `SELECT count(*), payload_hash, turn_id, tokens_in FROM agent_events WHERE event_id = ?`, ev.EventID).Scan(&count, &hash, &turn, &tokens); err != nil {
		t.Fatal(err)
	}
	if count != 1 || hash != ev.PayloadHash || turn != ev.TurnID || tokens != 0 {
		t.Fatalf("incomplete event: count=%d hash=%s turn=%s tokens=%d", count, hash, turn, tokens)
	}
	bs, err := blobs.NewStore(filepath.Join(path, ".semantica", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bs.Get(ctx, hash); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedDelivery_ReopensAfterRetentionAndSourceCleanup(t *testing.T) {
	ctx, ev, matches, src, root := retainedFixture(t)
	ids, err := RetainEvents(ctx, []RawEvent{ev}, matches, src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "source")); err != nil {
		t.Fatal(err)
	}
	if err := DrainRetained(ctx, ""); err != nil {
		t.Fatal(err)
	}
	for _, m := range matches {
		requireDeliveredEvent(t, ctx, m.Repo.Path, ev)
	}
	spool, _ := routingRoot()
	if _, err := os.Stat(filepath.Join(spool, "pending", ids[0]+".json")); !os.IsNotExist(err) {
		t.Fatalf("pending receipt remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(spool, "objects", ids[0])); !os.IsNotExist(err) {
		t.Fatalf("retained evidence remains: %v", err)
	}
	// A replay after offset loss needs no source blobs and cannot create another row.
	if _, err := RetainEvents(ctx, []RawEvent{ev}, matches, nil); err != nil {
		t.Fatal(err)
	}
	if err := DeliverRetained(ctx, ids, ""); err != nil {
		t.Fatal(err)
	}
	for _, m := range matches {
		requireDeliveredEvent(t, ctx, m.Repo.Path, ev)
	}
}

func TestRetainedDelivery_PartialFanoutAndBlobFailure(t *testing.T) {
	ctx, ev, matches, src, _ := retainedFixture(t)
	ids, err := RetainEvents(ctx, []RawEvent{ev}, matches, src)
	if err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(matches[1].Repo.Path, ".semantica", "objects")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := DeliverRetained(ctx, ids, ""); err == nil {
		t.Fatal("expected blob propagation failure")
	}
	requireDeliveredEvent(t, ctx, matches[0].Repo.Path, ev)
	spool, _ := routingRoot()
	r, err := readRetained(filepath.Join(spool, "pending", ids[0]+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Delivered) != 1 || !r.Delivered[matches[0].Repo.CanonicalPath] {
		t.Fatalf("wrong acknowledgements: %+v", r.Delivered)
	}
	// The acknowledged destination is no longer writable. Retry must only deliver B.
	if err := os.Remove(filepath.Join(matches[0].Repo.Path, ".semantica", "enabled")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := DrainRetained(ctx, ""); err != nil {
		t.Fatal(err)
	}
	requireDeliveredEvent(t, ctx, matches[1].Repo.Path, ev)
}

func TestRetainedDelivery_DBUnavailableAndUnacknowledgedWrite(t *testing.T) {
	ctx, ev, matches, src, _ := retainedFixture(t)
	matches = matches[:1]
	ids, err := RetainEvents(ctx, []RawEvent{ev}, matches, src)
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(matches[0].Repo.Path, ".semantica", "lineage.db")
	if err := os.Rename(db, db+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := DeliverRetained(ctx, ids, ""); err == nil {
		t.Fatal("missing DB was acknowledged")
	}
	if err := os.Rename(db+".saved", db); err != nil {
		t.Fatal(err)
	}
	// Simulate a process exit after the repo transaction but before receipt update.
	if _, err := PersistRoutedEvents(ctx, matches[0].Repo.Path, []RawEvent{ev}, src); err != nil {
		t.Fatal(err)
	}
	if err := DrainRetained(ctx, ""); err != nil {
		t.Fatal(err)
	}
	requireDeliveredEvent(t, ctx, matches[0].Repo.Path, ev)
}

func TestRetainedDelivery_RejectsIdentityMismatch(t *testing.T) {
	ctx, ev, matches, src, _ := retainedFixture(t)
	ids, err := RetainEvents(ctx, []RawEvent{ev}, matches, src)
	if err != nil {
		t.Fatal(err)
	}
	changed := ev
	changed.Summary = "different content"
	if _, err := RetainEvents(ctx, []RawEvent{changed}, matches, src); err == nil || !strings.Contains(err.Error(), "content mismatch") {
		t.Fatalf("mismatch accepted: %v", err)
	}
	if err := DeliverRetained(ctx, ids, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := RetainEvents(ctx, []RawEvent{changed}, matches, src); err == nil {
		t.Fatal("completed identity was reused")
	}
}

func TestRetainedDelivery_UnknownAndMissingEvidenceRemainPending(t *testing.T) {
	ctx, ev, _, src, _ := retainedFixture(t)
	ids, err := RetainEvents(ctx, []RawEvent{ev}, nil, src)
	if err != nil {
		t.Fatal(err)
	}
	if err := DeliverRetained(ctx, ids, ""); err != nil {
		t.Fatal(err)
	}
	spool, _ := routingRoot()
	if _, err := readRetained(filepath.Join(spool, "pending", ids[0]+".json")); err != nil {
		t.Fatal(err)
	}
	ev.EventID = "missing-evidence"
	ev.PayloadHash = strings.Repeat("a", 64)
	if _, err := RetainEvents(ctx, []RawEvent{ev}, nil, src); err == nil {
		t.Fatal("missing evidence was retained")
	}
	if _, err := os.Stat(filepath.Join(spool, "pending", routeID(ev.EventID)+".json")); !os.IsNotExist(err) {
		t.Fatal("receipt published without evidence")
	}
}

func TestPersistRoutedEvents_RepairsLegacyMissingHashRejectsConflict(t *testing.T) {
	ctx, ev, matches, src, _ := retainedFixture(t)
	path := matches[0].Repo.Path
	if _, err := WriteEventsToRepo(ctx, path, []RawEvent{ev}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := PersistRoutedEvents(ctx, path, []RawEvent{ev}, src); err != nil {
		t.Fatal(err)
	}
	requireDeliveredEvent(t, ctx, path, ev)
	ev.Summary = "conflict"
	if _, err := PersistRoutedEvents(ctx, path, []RawEvent{ev}, src); err == nil {
		t.Fatal("destination conflict accepted")
	}
}

func TestRetainedDelivery_ConcurrentReplayPreservesOneCopy(t *testing.T) {
	ctx, ev, matches, src, _ := retainedFixture(t)
	failures := make(chan error, 2)
	for range 2 {
		go func() {
			ids, err := RetainEvents(ctx, []RawEvent{ev}, matches, src)
			if err == nil {
				err = DeliverRetained(ctx, ids, "")
			}
			failures <- err
		}()
	}
	for range 2 {
		if err := <-failures; err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range matches {
		requireDeliveredEvent(t, ctx, m.Repo.Path, ev)
	}
}

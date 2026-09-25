package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/store/blobs"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/toolsnap"
	"github.com/semanticash/cli/internal/turncapture"
)

func attributionTurnFixture(repo, provider string) turncapture.Record {
	at := time.UnixMilli
	return turncapture.Record{
		Version: 1, Provider: provider, SessionID: "native", TurnID: "turn",
		StartedAt: at(2000), BaselineFinishedAt: at(2100),
		Repositories: []turncapture.RepoObservation{{
			Subject: turncapture.Subject{RepositoryID: repo},
			Baseline: toolsnap.TurnBaseline{
				Repository: toolsnap.RepoContext{HeadTree: "before"},
				Snapshot:   toolsnap.Snapshot{TreeHash: "before"},
				StartedAt:  at(2000), FinishedAt: at(2100),
			},
		}},
		End: &turncapture.End{
			BoundaryAt: at(3000), FinishedAt: at(3300), TrackedCompletion: "unknown",
			Repositories: []toolsnap.TurnObservation{{
				State: "changed", StartedAt: at(3100), FinishedAt: at(3200),
				Changes: []toolsnap.TurnChange{{
					BeforeTree: "before", Tree: "after",
					Files: []toolsnap.FileDelta{{
						Path: "generated.txt", Operation: "create", AfterMode: "100644",
						Hunks: []toolsnap.Hunk{{NewLines: []string{"generated"}}},
					}},
				}},
			}},
		},
	}
}

func TestTurnAttributionEligibility(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*turncapture.Record)
		want   int
	}{
		{"complete_observation_unknown_execution", func(*turncapture.Record) {}, 1},
		{"dirty_baseline_net_delta", func(r *turncapture.Record) {
			r.Repositories[0].Baseline.Snapshot.TreeHash = "dirty-before"
			r.End.Repositories[0].Changes[0].BeforeTree = "dirty-before"
		}, 1},
		{"running", func(r *turncapture.Record) { r.End.TrackedCompletion = "unsettled" }, 0},
		{"running_evidence", func(r *turncapture.Record) { r.End.Evidence = []turncapture.Evidence{{Kind: "inventory_running"}} }, 0},
		{"snapshot_failed", func(r *turncapture.Record) { r.End.Repositories[0].State = "unknown" }, 0},
		{"baseline_failed", func(r *turncapture.Record) { r.Repositories[0].Gap = "failed" }, 0},
		{"late_baseline", func(r *turncapture.Record) { r.BaselineFinishedAt = r.End.FinishedAt }, 0},
		{"foreign_repo", func(r *turncapture.Record) { r.Repositories[0].Subject.RepositoryID = "other" }, 0},
		{"foreign_provider", func(r *turncapture.Record) { r.Provider = "other" }, 0},
		{"foreign_turn", func(r *turncapture.Record) { r.TurnID = "other" }, 0},
		{"binary", func(r *turncapture.Record) { r.End.Repositories[0].Changes[0].Files[0].Binary = true }, 0},
		{"truncated", func(r *turncapture.Record) { r.End.Repositories[0].Changes[0].Files[0].Truncated = true }, 0},
		{"outside_repo", func(r *turncapture.Record) { r.End.Repositories[0].Changes[0].Files[0].Path = "../other" }, 0},
		{"unsupported_mode", func(r *turncapture.Record) { r.End.Repositories[0].Changes[0].Files[0].AfterMode = "120000" }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, h, repo := setupQueueRepo(t)
			ctx := context.Background()
			bs, err := blobs.NewStore(filepath.Join(dir, ".semantica", "objects"))
			if err != nil {
				t.Fatal(err)
			}
			src := insertSource(t, h, repo, "/source")
			session := insertSessionWithProvider(t, h, repo, src, "native", "codex")
			record := attributionTurnFixture(repo, "codex")
			tc.mutate(&record)
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			hash, _, err := bs.Put(ctx, raw)
			if err != nil {
				t.Fatal(err)
			}
			err = h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
				EventID: "obs", SessionID: session, RepositoryID: repo, Ts: 3000,
				Kind: "context", EventSource: "turn_observation", TurnID: sql.NullString{String: "turn", Valid: true},
			})
			if err != nil {
				t.Fatal(err)
			}
			err = h.Queries.InsertEvidenceLinkIfAbsent(ctx, sqldb.InsertEvidenceLinkIfAbsentParams{
				EventID: "obs", EvidenceKind: "turn_observation", EvidenceHash: hash, GroupID: "obs", CreatedAt: 3000,
			})
			if err != nil {
				t.Fatal(err)
			}
			insertPendingLinked(t, h, repo, "cp", "commit", 4000)
			got, err := loadTurnAttribution(ctx, h, bs, "cp", repo, "commit", tsWindow(1000, 4000))
			if err != nil || len(got.claims) != tc.want {
				t.Fatalf("claims=%v err=%v, want %d files", got.claims, err, tc.want)
			}
		})
	}
}

func TestTurnAttributionProvidersAndOverlap(t *testing.T) {
	for _, provider := range []string{"codex", "claude_code", "gemini", "copilot", "cursor", "kiro"} {
		t.Run(provider, func(t *testing.T) {
			dir, h, repo := setupQueueRepo(t)
			ctx := context.Background()
			bs, err := blobs.NewStore(filepath.Join(dir, ".semantica", "objects"))
			if err != nil {
				t.Fatal(err)
			}
			src := insertSource(t, h, repo, "/source")
			session := insertSessionWithProvider(t, h, repo, src, "native", provider)
			add := func(id string, end int64) {
				t.Helper()
				r := attributionTurnFixture(repo, provider)
				r.TurnID = id
				r.End.BoundaryAt = time.UnixMilli(end)
				r.End.FinishedAt = time.UnixMilli(end + 300)
				raw, err := json.Marshal(r)
				if err != nil {
					t.Fatal(err)
				}
				hash, _, err := bs.Put(ctx, raw)
				if err != nil {
					t.Fatal(err)
				}
				if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
					EventID: id, SessionID: session, RepositoryID: repo, Ts: end, Kind: "context",
					EventSource: "turn_observation", TurnID: sql.NullString{String: id, Valid: true},
				}); err != nil {
					t.Fatal(err)
				}
				if err := h.Queries.InsertEvidenceLinkIfAbsent(ctx, sqldb.InsertEvidenceLinkIfAbsentParams{
					EventID: id, EvidenceKind: "turn_observation", EvidenceHash: hash, GroupID: id, CreatedAt: end,
				}); err != nil {
					t.Fatal(err)
				}
			}
			add("first", 3000)
			insertPendingLinked(t, h, repo, "cp", "commit", 4000)
			got, err := loadTurnAttribution(ctx, h, bs, "cp", repo, "commit", tsWindow(1000, 4000))
			if err != nil || len(got.claims["generated.txt"]) != 1 || got.claims["generated.txt"][0].Provider != provider {
				t.Fatalf("missing provider credit: %+v %v", got, err)
			}
			// A later publication can reveal overlap with the selected turn.
			add("second", 5000)
			got, err = loadTurnAttribution(ctx, h, bs, "cp", repo, "commit", tsWindow(1000, 4000))
			if err != nil || len(got.claims) != 0 {
				t.Fatalf("overlapping turns received credit: %+v %v", got, err)
			}
		})
	}
}

func TestAttributionTurnCommitSelection(t *testing.T) {
	r := attributionTurnFixture("repo", "codex")
	final := r.End.Repositories[0].Changes[0]
	first, second := final, final
	first.Commit, second.Commit = "first", "second"
	r.End.Repositories[0].Changes = []toolsnap.TurnChange{first, second, final}
	for _, commit := range []string{"first", "second"} {
		if got := attributionTurnChange(&r, commit, false); got == nil || got.Commit != commit {
			t.Fatalf("lost intermediate commit %s", commit)
		}
	}
	if got := attributionTurnChange(&r, "unrelated", true); got != nil {
		t.Fatal("reused aggregate end state for a later commit")
	}
	r.Repositories[0].Baseline.Snapshot.TreeHash = "preexisting-dirty"
	if got := attributionTurnChange(&r, "first", false); got != nil {
		t.Fatal("credited pre-turn dirt from a commit delta")
	}
}

func TestTurnAttributionLateWorkerWithoutWindowEvents(t *testing.T) {
	dir, h, repo := setupQueueRepo(t)
	ctx := context.Background()
	bs, err := blobs.NewStore(filepath.Join(dir, ".semantica", "objects"))
	if err != nil {
		t.Fatal(err)
	}
	src := insertSource(t, h, repo, "/source")
	session := insertSessionWithProvider(t, h, repo, src, "native", "codex")
	insertPendingLinked(t, h, repo, "cp", "commit", 2500)
	r := attributionTurnFixture(repo, "codex")
	r.End.Repositories[0].Changes[0].Commit = "commit"
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	hash, _, err := bs.Put(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: "obs", SessionID: session, RepositoryID: repo, Ts: 3000, Kind: "context",
		EventSource: "turn_observation", TurnID: sql.NullString{String: "turn", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertEvidenceLinkIfAbsent(ctx, sqldb.InsertEvidenceLinkIfAbsentParams{
		EventID: "obs", EvidenceKind: "turn_observation", EvidenceHash: hash, GroupID: "commit", CreatedAt: 3000,
	}); err != nil {
		t.Fatal(err)
	}
	diff := []byte("diff --git a/generated.txt b/generated.txt\nnew file mode 100644\n--- /dev/null\n+++ b/generated.txt\n@@ -0,0 +1 @@\n+generated\n")
	got, err := attributeWithCarryForward(ctx, h, bs, diff, ComputeAIPercentInput{
		RepoRoot: dir, RepoID: repo, CheckpointID: "cp", CommitHash: "commit", Window: tsWindow(1000, 2500),
	}, nil, filepath.Join(dir, ".semantica"), true)
	if err != nil || got.noEvents || got.result.Percent != 100 {
		t.Fatalf("late observation lost from worker scoring: %+v %v", got, err)
	}
}

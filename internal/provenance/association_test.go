package provenance

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func TestRepositoryAssociationSurvivesUploadPreparation(t *testing.T) {
	for _, test := range []struct {
		name     string
		observed bool
		tool     bool
	}{
		{"observation_only", true, false},
		{"observation_and_tool", true, true},
		{"different_turn", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			repo, semDir, store := newSyncRepo(t, ctx)
			h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = sqlstore.Close(h) }()
			var repoID, sessionID string
			if err := h.DB.QueryRow("select repository_id, session_id from agent_sessions where provider_session_id='ps'").Scan(&repoID, &sessionID); err != nil {
				t.Fatal(err)
			}
			turn := "other-turn"
			if test.observed {
				turn = "turn-1"
			}
			if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
				EventID: "observation", SessionID: sessionID, RepositoryID: repoID,
				Ts: 2, TurnID: sqlstore.NullStr(turn), Kind: "context", EventSource: "turn_observation",
				Role: sqlstore.NullStr("system"), ToolUses: sqlstore.NullStr(`{"mutation_routing":"context_only"}`),
			}); err != nil {
				t.Fatal(err)
			}
			if test.tool {
				if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
					EventID: "tool", SessionID: sessionID, RepositoryID: repoID,
					Ts: 2, TurnID: sqlstore.NullStr("turn-1"), Kind: "assistant", EventSource: "hook",
					Role: sqlstore.NullStr("assistant"), ToolName: sqlstore.NullStr("Write"),
				}); err != nil {
					t.Fatal(err)
				}
			}
			promptHash, _, err := store.Put(ctx, []byte("Change B"))
			if err != nil {
				t.Fatal(err)
			}
			response := RedactAndStoreResponse(ctx, store, "response", "Changed B.", 3)
			PackageTurn(ctx, repo, TurnContext{
				Provider: "codex", SessionID: "ps", TurnID: "turn-1", CWD: repo,
				StartedAt: 1, CompletedAt: 3, Prompt: PromptCandidate{EventID: "prompt", Hash: promptHash},
				ResponseCandidate: response,
			}, store)
			results, err := SyncPendingTurns(ctx, repo, 0, 50)
			if err != nil || len(results) != 1 || results[0].Skipped {
				t.Fatalf("prepare upload: %+v %v", results, err)
			}
			var envelope syncEnvelope
			if err := json.Unmarshal(results[0].Envelope, &envelope); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, object := range envelope.Objects {
				if object.Kind == "turn_observation" {
					t.Fatal("local snapshot included in upload")
				}
				if object.Kind != "bundle" {
					continue
				}
				found = true
				var bundle provenanceBundle
				if err := json.Unmarshal(results[0].RedactedBlobs[object.Hash], &bundle); err != nil {
					t.Fatal(err)
				}
				if bundle.Prompt == nil || bundle.Response == nil || bundle.Response.Status != "complete" {
					t.Fatal("prompt/response missing")
				}
				if test.observed {
					want := bundleRepositoryAssociation{Basis: "turn_observation", Authorship: "unknown"}
					if bundle.RepositoryAssociation == nil || *bundle.RepositoryAssociation != want {
						t.Fatalf("association lost: %+v", bundle.RepositoryAssociation)
					}
				} else if bundle.RepositoryAssociation != nil {
					t.Fatal("another turn's observation leaked into bundle")
				}
				wantSteps := 0
				if test.tool {
					wantSteps = 1
				}
				if len(bundle.Steps) != wantSteps {
					t.Fatalf("tool steps changed: %d", len(bundle.Steps))
				}
			}
			if !found {
				t.Fatal("prepared bundle missing")
			}
		})
	}
}

package hooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
)

func TestProviderOrchestrationEventsReachSessionRepo(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)
	w := newToolWindowWorld(t, home, "repo")
	bs, err := blobs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repos, err := broker.ListActiveRepos(ctx, w.bh)
	if err != nil {
		t.Fatal(err)
	}
	h, err := sqlstore.Open(ctx, filepath.Join(w.semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	for _, provider := range []string{"claude", "gemini", "kirocli", "copilot", "cursor"} {
		paths, err := filepath.Glob(filepath.Join(provider, "testdata", "direct_emit", "subagent*.golden.json"))
		if err != nil || len(paths) == 0 {
			t.Fatalf("%s fixtures: %v %v", provider, paths, err)
		}
		for _, path := range paths {
			t.Run(path, func(t *testing.T) {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var fixture struct {
					Events []broker.RawEvent `json:"expected_events"`
					Blobs  map[string]string `json:"expected_blobs"`
				}
				if err := json.Unmarshal(data, &fixture); err != nil {
					t.Fatal(err)
				}
				if len(fixture.Events) == 0 {
					t.Fatal("fixture has no boundary events")
				}
				for _, value := range fixture.Blobs {
					if _, _, err := bs.Put(ctx, []byte(value)); err != nil {
						t.Fatal(err)
					}
				}
				for i := range fixture.Events {
					ev := &fixture.Events[i]
					if ev.ToolName != "Agent" {
						t.Fatalf("not an orchestration event: %+v", ev)
					}
					ev.SourceProjectPath = w.repoPath
					// Fixture identities may overlap across completion variants.
					ev.EventID = path + ":" + ev.EventID
				}
				if err := routeAndWriteEventsToRepos(ctx, fixture.Events, repos, bs); err != nil {
					t.Fatal(err)
				}
				for _, ev := range fixture.Events {
					var tool string
					var turn sql.NullString
					if err := h.DB.QueryRow("SELECT tool_name, turn_id FROM agent_events WHERE event_id=?", ev.EventID).Scan(&tool, &turn); err != nil {
						t.Fatal(err)
					}
					if tool != "Agent" || turn.String != ev.TurnID {
						t.Fatalf("lost context: tool=%s turn=%+v", tool, turn)
					}
				}
			})
		}
	}
	root, err := broker.UnresolvedMutationDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("orchestration created unresolved archive: %v", err)
	}
}

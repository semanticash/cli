package provenance

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/semanticash/cli/internal/store/blobs"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

func TestBundlePreservesObservationContextAndDelta(t *testing.T) {
	ctx := context.Background()
	bs, err := blobs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := makeStep("event", "Bash", "tool", "hook")
	s.ToolUses = sql.NullString{Valid: true, String: `{"mutation_routing":"context_only","tools":[{"name":"Bash","file_path":"/repo/victim.txt"}]}`}
	filtered := filterIgnoredSteps(ctx, "/repo", []sqldb.ListStepEventsForTurnRow{s}, bs)
	if len(filtered) != 1 || len(filtered[0].FilePaths) != 0 {
		t.Fatalf("context claims paths: %+v", filtered)
	}
	hash, _, err := buildProvenanceBundleFromFiltered(ctx, bs, TurnContext{TurnID: "turn"}, sqldb.AgentSession{SessionID: "session"}, nil, filtered, map[string]string{"event": "delta"}, ResponseCandidate{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := bs.Get(ctx, hash)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = RedactForUpload(raw, "bundle", "/repo")
	if err != nil {
		t.Fatal(err)
	}
	var bundle provenanceBundle
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatal(err)
	}
	if len(bundle.Steps) != 1 {
		t.Fatalf("steps=%+v", bundle.Steps)
	}
	step := bundle.Steps[0]
	if step.MutationRouting != "context_only" || step.DeltaHash != "delta" || len(step.FilePaths) != 0 || step.EventID != "event" {
		t.Fatalf("lost routing context: %+v", step)
	}
}

package cursor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// The Cursor hook lifecycle stores the final response in the repository CAS.
func TestCursorLifecycle_ResponseCapturedAndResolvableInRepoCAS(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("SEMANTICA_HOME", home)

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repoPath := filepath.Join(base, "repo")
	semDir := filepath.Join(repoPath, ".semantica")
	if err := os.MkdirAll(filepath.Join(semDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
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
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: uuid.NewString(), RootPath: repoPath, CreatedAt: 1000, EnabledAt: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	_ = sqlstore.Close(h)

	bh, err := broker.Open(ctx, filepath.Join(home, "repos.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close(bh) }()
	if err := broker.Register(ctx, bh, repoPath, repoPath); err != nil {
		t.Fatal(err)
	}

	// Hooks write to the global store before packaging copies objects to the repo.
	global, err := blobs.NewStore(filepath.Join(home, "objects"))
	if err != nil {
		t.Fatal(err)
	}

	// Use an empty transcript so replay succeeds without additional events.
	transcript := filepath.Join(home, "transcript.jsonl")
	if err := os.WriteFile(transcript, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	cwd, tr := q(repoPath), q(transcript)

	p := &Provider{}
	dispatch := func(hookName, payload string) {
		t.Helper()
		ev, err := p.ParseHookEvent(ctx, hookName, strings.NewReader(payload))
		if err != nil {
			t.Fatalf("parse %s: %v", hookName, err)
		}
		if ev == nil {
			t.Fatalf("nil event for %s", hookName)
		}
		if err := hooks.Dispatch(ctx, p, ev, bh, global); err != nil {
			t.Fatalf("dispatch %s: %v", hookName, err)
		}
	}

	const conv = "conv-e2e"
	dispatch("before-submit-prompt", `{"conversation_id":"`+conv+`","cwd":`+cwd+`,"transcript_path":`+tr+`,"prompt":"do it"}`)
	dispatch("after-agent-response", `{"conversation_id":"`+conv+`","cwd":`+cwd+`,"transcript_path":`+tr+`,"text":"the final answer"}`)
	dispatch("stop", `{"conversation_id":"`+conv+`","cwd":`+cwd+`,"transcript_path":`+tr+`}`)

	// The manifest records the response object.
	h2, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h2) }()
	var status, hash string
	if err := h2.DB.QueryRowContext(ctx,
		"select response_status, response_hash from provenance_manifests where provider = 'cursor' and kind = 'turn_bundle'").
		Scan(&status, &hash); err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if status != "complete" || hash == "" {
		t.Fatalf("manifest response = (%q, %q), want complete with a hash", status, hash)
	}

	// The response object must resolve in the repository CAS.
	repoStore, err := blobs.NewStore(filepath.Join(semDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := repoStore.Get(ctx, hash)
	if err != nil {
		t.Fatalf("response object not resolvable in repo CAS: %v", err)
	}
	if !strings.Contains(string(raw), "the final answer") {
		t.Errorf("stored object missing response text: %s", raw)
	}
}

package health

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/broker"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/toolsnap"
)

// seedRepoRow registers the repository row event writes require.
func seedRepoRow(t *testing.T, ctx context.Context, dir string) {
	t.Helper()
	h, err := sqlstore.Open(ctx, filepath.Join(dir, ".semantica", "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlstore.Close(h) }()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: "repo-tw", RootPath: dir, CreatedAt: 1, EnabledAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
}

func enabledRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	semDir := filepath.Join(dir, ".semantica")
	if err := os.MkdirAll(semDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCheckToolWindows(t *testing.T) {
	ctx := context.Background()

	t.Run("disabled repo is silent", func(t *testing.T) {
		dir := t.TempDir()
		if checks := checkToolWindows(ctx, Options{RepoPath: dir}); len(checks) != 0 {
			t.Fatalf("checks = %+v, want none", checks)
		}
	})

	t.Run("no registry yet", func(t *testing.T) {
		dir := enabledRepo(t)
		checks := checkToolWindows(ctx, Options{RepoPath: dir})
		if c := findCheck(t, checks, "registry"); c.Status != StatusOK {
			t.Fatalf("checks = %+v, want OK registry", checks)
		}
		// Health checks do not create registry state.
		if _, err := os.Stat(filepath.Join(dir, ".semantica", "tool-windows")); !os.IsNotExist(err) {
			t.Fatalf("doctor created the registry: %v", err)
		}
	})

	t.Run("recovery backlog warns", func(t *testing.T) {
		dir := enabledRepo(t)
		reg, err := toolsnap.OpenRegistry(filepath.Join(dir, ".semantica"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reg.LoadOrRecordPendingPartial(toolsnap.PendingPartialRecord{
			Key: toolsnap.ToolKey{
				RepositoryID: "r1", Provider: "claude_code",
				SessionID: "s1", TurnID: "t1", ToolUseID: "tu1",
			},
			EventID: strings.Repeat("ab", 32), Reason: "pre_snapshot_missing",
			ToolName: "Bash", Timestamp: 5000,
		}); err != nil {
			t.Fatal(err)
		}
		checks := checkToolWindows(ctx, Options{RepoPath: dir})
		if c := findCheck(t, checks, "recovery"); c.Status != StatusWarn || !strings.Contains(c.Message, "1 recoverable") {
			t.Fatalf("checks = %+v, want recovery warning", checks)
		}
	})

	t.Run("degraded groups warn before retention without claiming capture stopped", func(t *testing.T) {
		dir := enabledRepo(t)
		reg, err := toolsnap.OpenRegistry(filepath.Join(dir, ".semantica"))
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UnixMilli()
		mkKey := func(tool string) toolsnap.ToolKey {
			return toolsnap.ToolKey{
				RepositoryID: "r1", Provider: "claude_code",
				SessionID: "s1", TurnID: "t1", ToolUseID: tool,
			}
		}
		// Create an expired group with active and completed members.
		created := now - 2*toolsnap.DefaultStaleActiveAge.Milliseconds()
		stuck := toolsnap.PendingToolSnapshot{
			Key: mkKey("tu-stuck"), ToolName: "Bash",
			SnapshotRef: "refs/semantica/tool-windows/x", TreeHash: "t", HeadHash: "h",
			ObjectFormat: "sha1", StartedAt: created,
		}
		if _, err := reg.Begin(ctx, stuck); err != nil {
			t.Fatal(err)
		}
		blocked := stuck
		blocked.Key = mkKey("tu-blocked")
		blocked.StartedAt = created + 1
		if _, err := reg.Begin(ctx, blocked); err != nil {
			t.Fatal(err)
		}
		// Complete one member while the group remains open.
		if _, err := reg.Complete(ctx, blocked.Key,
			toolsnap.CompletionInfo{EventID: strings.Repeat("cd", 32), At: created + 2},
			nil, func([]toolsnap.PendingToolSnapshot, *toolsnap.GroupFinal, bool, func() error) (toolsnap.FinalizeResult, error) {
				t.Fatal("finalize invoked for an open group")
				return toolsnap.FinalizeResult{}, nil
			}); err != nil {
			t.Fatal(err)
		}

		checks := checkToolWindows(ctx, Options{RepoPath: dir})
		c := findCheck(t, checks, "recovery")
		if c.Status != StatusWarn || !strings.Contains(c.Message, "degraded group") {
			t.Fatalf("checks = %+v, want degraded-group warning", checks)
		}
		if !strings.Contains(c.Message, "1 completed member") {
			t.Fatalf("message = %q, want blocked member count", c.Message)
		}
		if !strings.Contains(c.Message, "fresh groups") {
			t.Fatalf("message = %q, want reassurance that fresh captures are unaffected", c.Message)
		}
	})

	t.Run("drift ignores missing-pre links", func(t *testing.T) {
		dir := enabledRepo(t)
		t.Setenv("SEMANTICA_HOME", filepath.Join(dir, ".semantica-global"))
		if err := sqlstore.MigratePath(ctx, filepath.Join(dir, ".semantica", "lineage.db")); err != nil {
			t.Fatal(err)
		}
		seedRepoRow(t, ctx, dir)
		evtID := strings.Repeat("ab", 32)
		if _, err := broker.WriteEventsToRepo(ctx, dir, []broker.RawEvent{{
			EventID: evtID, SourceKey: "/data/s.jsonl", Provider: "claude_code",
			Timestamp: time.Now().UnixMilli(), Kind: "assistant", Role: "assistant",
			ToolUseID: "toolu_1", ToolName: "Bash", EventSource: "hook",
			ProviderSessionID: "s1", SessionStartedAt: 1,
			SessionMetaJSON: `{"source_key":"x"}`,
		}}, nil); err != nil {
			t.Fatal(err)
		}
		// Missing-pre evidence does not satisfy the drift check.
		if err := broker.WriteEvidenceLinksToRepo(ctx, dir, []broker.EvidenceLink{{
			EventID: evtID, EvidenceKind: "tool_delta", EvidenceHash: "h1",
			GroupID: "pre_snapshot_missing:" + evtID, CreatedAt: time.Now().UnixMilli(),
		}}); err != nil {
			t.Fatal(err)
		}
		checks := checkToolWindows(ctx, Options{RepoPath: dir})
		if c := findCheck(t, checks, "drift"); c.Status != StatusWarn {
			t.Fatalf("checks = %+v, want drift warning despite missing-pre link", checks)
		}

		// Captured evidence clears this event's warning.
		if err := broker.WriteEvidenceLinksToRepo(ctx, dir, []broker.EvidenceLink{{
			EventID: evtID, EvidenceKind: "tool_delta", EvidenceHash: "h2",
			GroupID: "g-real", CreatedAt: time.Now().UnixMilli(),
		}}); err != nil {
			t.Fatal(err)
		}
		for _, c := range checkToolWindows(ctx, Options{RepoPath: dir}) {
			if c.ID == "drift" {
				t.Fatalf("drift still reported with captured evidence: %+v", c)
			}
		}

		// A later unmatched event still reports drift.
		evt2 := strings.Repeat("cd", 32)
		if _, err := broker.WriteEventsToRepo(ctx, dir, []broker.RawEvent{{
			EventID: evt2, SourceKey: "/data/s.jsonl", Provider: "claude_code",
			Timestamp: time.Now().UnixMilli(), Kind: "assistant", Role: "assistant",
			ToolUseID: "toolu_2", ToolName: "Bash", EventSource: "hook",
			ProviderSessionID: "s1", SessionStartedAt: 1,
			SessionMetaJSON: `{"source_key":"x"}`,
		}}, nil); err != nil {
			t.Fatal(err)
		}
		checks = checkToolWindows(ctx, Options{RepoPath: dir})
		c := findCheck(t, checks, "drift")
		if c.Status != StatusWarn || !strings.Contains(c.Message, "1 Bash hook event") {
			t.Fatalf("checks = %+v, want per-event drift warning", checks)
		}
		// Captured evidence in the window redirects the diagnosis to
		// stale sessions rather than missing installation.
		if !strings.Contains(c.Message, "1 captured") ||
			!strings.Contains(c.Remediation, "restart agent sessions") {
			t.Fatalf("check = %+v, want the stale-session diagnosis", c)
		}

		// Providers without a pre hook do not count as drift.
		evtCodex := strings.Repeat("ef", 32)
		if _, err := broker.WriteEventsToRepo(ctx, dir, []broker.RawEvent{{
			EventID: evtCodex, SourceKey: "/data/c.jsonl", Provider: "codex",
			Timestamp: time.Now().UnixMilli(), Kind: "assistant", Role: "assistant",
			ToolUseID: "exec-1", ToolName: "Bash", EventSource: "hook",
			ProviderSessionID: "c1", SessionStartedAt: 1,
			SessionMetaJSON: `{"source_key":"x"}`,
		}}, nil); err != nil {
			t.Fatal(err)
		}
		checks = checkToolWindows(ctx, Options{RepoPath: dir})
		c = findCheck(t, checks, "drift")
		if !strings.Contains(c.Message, "1 Bash hook event") {
			t.Fatalf("checks = %+v, want codex event excluded from drift", checks)
		}

		// Pending groups suppress drift until closure.
		reg, err := toolsnap.OpenRegistry(filepath.Join(dir, ".semantica"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reg.Begin(ctx, toolsnap.PendingToolSnapshot{
			Key: toolsnap.ToolKey{
				RepositoryID: "repo-tw", Provider: "claude_code",
				SessionID: "s1", TurnID: "t1", ToolUseID: "toolu_open",
			},
			ToolName: "Bash", SnapshotRef: "refs/semantica/tool-windows/x",
			TreeHash: "t", HeadHash: "h", ObjectFormat: "sha1",
			StartedAt: time.Now().UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
		for _, c := range checkToolWindows(ctx, Options{RepoPath: dir}) {
			if c.ID == "drift" {
				t.Fatalf("drift reported while a group is open: %+v", c)
			}
		}
	})

	t.Run("reclaimed partials count as captured, not drift", func(t *testing.T) {
		dir := enabledRepo(t)
		t.Setenv("SEMANTICA_HOME", filepath.Join(dir, ".semantica-global"))
		if err := sqlstore.MigratePath(ctx, filepath.Join(dir, ".semantica", "lineage.db")); err != nil {
			t.Fatal(err)
		}
		seedRepoRow(t, ctx, dir)
		evtID := strings.Repeat("ab", 32)
		if _, err := broker.WriteEventsToRepo(ctx, dir, []broker.RawEvent{{
			EventID: evtID, SourceKey: "/data/s.jsonl", Provider: "claude_code",
			Timestamp: time.Now().UnixMilli(), Kind: "assistant", Role: "assistant",
			ToolUseID: "toolu_rc", ToolName: "Bash", EventSource: "hook",
			ProviderSessionID: "s1", SessionStartedAt: 1,
			SessionMetaJSON: `{"source_key":"x"}`,
		}}, nil); err != nil {
			t.Fatal(err)
		}
		// A reclaimed window proves the pre hook fired.
		if err := broker.WriteEvidenceLinksToRepo(ctx, dir, []broker.EvidenceLink{{
			EventID: evtID, EvidenceKind: "tool_delta", EvidenceHash: "h1",
			GroupID: "stale_active_window:" + evtID, CreatedAt: time.Now().UnixMilli(),
		}}); err != nil {
			t.Fatal(err)
		}
		for _, c := range checkToolWindows(ctx, Options{RepoPath: dir}) {
			if c.ID == "drift" {
				t.Fatalf("reclaimed partial reported as drift: %+v", c)
			}
		}
	})

	t.Run("near-match reason prefix is not read as missing-pre drift", func(t *testing.T) {
		dir := enabledRepo(t)
		t.Setenv("SEMANTICA_HOME", filepath.Join(dir, ".semantica-global"))
		if err := sqlstore.MigratePath(ctx, filepath.Join(dir, ".semantica", "lineage.db")); err != nil {
			t.Fatal(err)
		}
		seedRepoRow(t, ctx, dir)
		evtID := strings.Repeat("ab", 32)
		if _, err := broker.WriteEventsToRepo(ctx, dir, []broker.RawEvent{{
			EventID: evtID, SourceKey: "/data/s.jsonl", Provider: "claude_code",
			Timestamp: time.Now().UnixMilli(), Kind: "assistant", Role: "assistant",
			ToolUseID: "toolu_nm", ToolName: "Bash", EventSource: "hook",
			ProviderSessionID: "s1", SessionStartedAt: 1,
			SessionMetaJSON: `{"source_key":"x"}`,
		}}, nil); err != nil {
			t.Fatal(err)
		}
		// A near-match reason counts as captured evidence.
		if err := broker.WriteEvidenceLinksToRepo(ctx, dir, []broker.EvidenceLink{{
			EventID: evtID, EvidenceKind: "tool_delta", EvidenceHash: "h1",
			GroupID: "preXsnapshotYmissing:" + evtID, CreatedAt: time.Now().UnixMilli(),
		}}); err != nil {
			t.Fatal(err)
		}
		for _, c := range checkToolWindows(ctx, Options{RepoPath: dir}) {
			if c.ID == "drift" {
				t.Fatalf("near-match reason misclassified as missing-pre drift: %+v", c)
			}
		}
	})

	t.Run("malformed tombstone fails", func(t *testing.T) {
		dir := enabledRepo(t)
		if _, err := toolsnap.OpenRegistry(filepath.Join(dir, ".semantica")); err != nil {
			t.Fatal(err)
		}
		tombs := filepath.Join(dir, ".semantica", "tool-windows", "tombstones")
		if err := os.WriteFile(filepath.Join(tombs, "garbage"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		checks := checkToolWindows(ctx, Options{RepoPath: dir})
		if c := findCheck(t, checks, "tombstones"); c.Status != StatusFail {
			t.Fatalf("checks = %+v, want tombstone failure", checks)
		}
	})
}

package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	attrreporting "github.com/semanticash/cli/internal/attribution/reporting"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/toolsnap"
)

// v2Repo provides a Git repository and Semantica state for delta tests.
type v2Repo struct {
	t      *testing.T
	dir    string
	semDir string
	h      *sqlstore.Handle
	bs     *blobs.Store
	repoID string
	root   string
}

func newV2Repo(t *testing.T) *v2Repo {
	t.Helper()
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	t.Setenv("SEMANTICA_HOME", filepath.Join(dir, ".semantica-global"))
	t.Setenv("HOME", dir)

	r := &v2Repo{t: t, dir: dir, semDir: filepath.Join(dir, ".semantica")}
	r.git("init")
	r.git("config", "user.email", "test@test.com")
	r.git("config", "user.name", "Test")
	mustWriteFile(t, filepath.Join(dir, "README"), []byte("init\n"))
	r.git("add", "README")
	r.git("commit", "-m", "initial")

	mustMkdirAll(t, r.semDir)
	mustWriteFile(t, filepath.Join(r.semDir, "enabled"), nil)
	mustMkdirAll(t, filepath.Join(r.semDir, "objects"))

	ctx := context.Background()
	h, err := sqlstore.Open(ctx, filepath.Join(r.semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	r.h = h
	repo := mustOpenRepo(t, dir)
	r.root = repo.Root()
	r.repoID = uuid.NewString()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: r.repoID, RootPath: r.root,
		CreatedAt: 100_000, EnabledAt: 100_000,
	}); err != nil {
		t.Fatal(err)
	}
	baselineID := uuid.NewString()
	if err := h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: baselineID, RepositoryID: r.repoID,
		CreatedAt: 100_000, Kind: "baseline", Status: "complete",
		CompletedAt: sql.NullInt64{Int64: 100_000, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	bs, err := blobs.NewStore(filepath.Join(r.semDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	r.bs = bs
	return r
}

func (r *v2Repo) git(args ...string) {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func (r *v2Repo) session(provider string) string {
	r.t.Helper()
	srcID := insertSource(r.t, r.h, r.repoID, "/data/"+uuid.NewString()+".jsonl")
	return insertSessionWithProvider(r.t, r.h, r.repoID, srcID, uuid.NewString(), provider)
}

func (r *v2Repo) bashEvent(sessID string, ts int64) string {
	r.t.Helper()
	ctx := context.Background()
	eventID := uuid.NewString()
	payload := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"make generate"}}]}}`
	payloadHash, _, err := r.bs.Put(ctx, []byte(payload))
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: eventID, SessionID: sessID, RepositoryID: r.repoID, Ts: ts,
		Kind: "assistant", Role: sqlstore.NullStr("assistant"),
		ToolUses:    sql.NullString{String: `{"content_types":["tool_use"],"tools":[{"name":"Bash"}]}`, Valid: true},
		PayloadHash: sqlstore.NullStr(payloadHash),
	}); err != nil {
		r.t.Fatal(err)
	}
	return eventID
}

func (r *v2Repo) linkDelta(delta *toolsnap.Delta, eventIDs []string, ts int64) {
	r.t.Helper()
	ctx := context.Background()
	raw, err := delta.CanonicalBytes()
	if err != nil {
		r.t.Fatal(err)
	}
	hash, _, err := r.bs.Put(ctx, raw)
	if err != nil {
		r.t.Fatal(err)
	}
	groupID := uuid.NewString()
	for _, eid := range eventIDs {
		if err := r.h.Queries.InsertEvidenceLinkIfAbsent(ctx, sqldb.InsertEvidenceLinkIfAbsentParams{
			EventID: eid, EvidenceKind: "tool_delta",
			EvidenceHash: hash, GroupID: groupID, CreatedAt: ts,
		}); err != nil {
			r.t.Fatal(err)
		}
	}
}

// commitLinked commits paths and links the commit to a checkpoint.
func (r *v2Repo) commitLinked(ts int64, paths ...string) string {
	r.t.Helper()
	ctx := context.Background()
	r.git(append([]string{"add"}, paths...)...)
	r.git("commit", "-m", "fixture commit")
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = r.dir
	out, err := cmd.Output()
	if err != nil {
		r.t.Fatal(err)
	}
	commitHash := strings.TrimSpace(string(out))
	cpID := uuid.NewString()
	if err := r.h.Queries.InsertCheckpoint(ctx, sqldb.InsertCheckpointParams{
		CheckpointID: cpID, RepositoryID: r.repoID,
		CreatedAt: ts, Kind: "auto", Status: "complete",
		CompletedAt: sql.NullInt64{Int64: ts, Valid: true},
	}); err != nil {
		r.t.Fatal(err)
	}
	if err := r.h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: commitHash, RepositoryID: r.repoID,
		CheckpointID: cpID, LinkedAt: ts,
	}); err != nil {
		r.t.Fatal(err)
	}
	return commitHash
}

func (r *v2Repo) close() {
	_ = sqlstore.Close(r.h)
}

func toolUse(eventID, toolUseID string, actor int) toolsnap.ToolUse {
	return toolsnap.ToolUse{ToolUseID: toolUseID, ToolName: "Bash", EventID: eventID, Actor: actor}
}

// A formatter delta attributes lines that no longer match the original edit.
func TestV2Regression_GofmtSpecimen(t *testing.T) {
	r := newV2Repo(t)
	defer r.close()
	sessID := r.session("claude_code")

	// Direct evidence contains the unformatted source.
	preFormat := "package fmtspec\nvar  x  =  1\nfunc Compact() { return }\n"
	insertEventWithPayload(t, r.h, r.bs, sessID, r.repoID, r.root, 200_000, "fmtspec.go", preFormat)

	// The tool delta contains the committed gofmt output.
	postLines := []string{"package fmtspec", "var x = 1", "func Compact() {", "return", "}"}
	eventID := r.bashEvent(sessID, 210_000)
	r.linkDelta(&toolsnap.Delta{
		Scope: "tool", Status: "complete",
		Window:   toolsnap.Window{StartedAt: 209_000, CompletedAt: 210_000, DurationMS: 1000},
		Actors:   []toolsnap.Actor{{Provider: "claude_code", SessionID: sessID, TurnID: "t1"}},
		ToolUses: []toolsnap.ToolUse{toolUse(eventID, "toolu_fmt", 0)},
		Files: []toolsnap.FileDelta{{
			Path: "fmtspec.go", Operation: "edit",
			BeforeHash: "a", AfterHash: "b",
			BeforeMode: "100644", AfterMode: "100644",
			Hunks: []toolsnap.Hunk{{
				OldStart: 1, OldCount: 0, NewStart: 1, NewCount: len(postLines),
				NewLines: postLines,
			}},
		}},
		Limits: toolsnap.Limits{FilesObserved: 1},
	}, []string{eventID}, 210_000)

	mustWriteFile(t, filepath.Join(r.dir, "fmtspec.go"),
		[]byte("package fmtspec\nvar x = 1\nfunc Compact() {\n\treturn\n}\n"))
	commitHash := r.commitLinked(300_000, "fmtspec.go")

	svc := NewAttributionService()
	ctx := context.Background()

	// Direct matching treats the reflowed lines as modified.
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "0")
	v1, err := svc.AttributeCommit(ctx, AttributionInput{RepoPath: r.dir, CommitHash: commitHash})
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	if v1.AIExactLines != 1 || v1.AIFormattedLines != 1 || v1.AIModifiedLines != 3 {
		t.Fatalf("v1 = %d/%d/%d exact/formatted/modified, want 1/1/3",
			v1.AIExactLines, v1.AIFormattedLines, v1.AIModifiedLines)
	}

	// Delta matching identifies every formatted line exactly.
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "1")
	v2, err := svc.AttributeCommit(ctx, AttributionInput{RepoPath: r.dir, CommitHash: commitHash})
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	if v2.AIExactLines != 5 || v2.AIModifiedLines != 0 || v2.AIFormattedLines != 0 {
		t.Fatalf("v2 = %d/%d/%d exact/formatted/modified, want 5/0/0",
			v2.AIExactLines, v2.AIFormattedLines, v2.AIModifiedLines)
	}
	if v2.AIDeltaExactLines != 5 {
		t.Fatalf("v2 delta exact = %d, want 5", v2.AIDeltaExactLines)
	}
	if v2.AILines != 5 || v1.AILines != 5 {
		t.Fatalf("headline drifted: v1 %d, v2 %d, want 5/5 (quality upgrade, not double count)",
			v1.AILines, v2.AILines)
	}
	if v2.Files[0].EvidenceClass != string(attrreporting.EvidenceExact) {
		t.Fatalf("evidence = %q, want exact", v2.Files[0].EvidenceClass)
	}
	if got := v2.ProviderDetails[0].AILines; got != 5 {
		t.Fatalf("provider lines = %d, want 5 (single provider, no double count)", got)
	}
}

// Delta evidence attributes a reflow with no surviving direct anchor.
func TestV2Regression_ReflowWithoutAnchor(t *testing.T) {
	r := newV2Repo(t)
	defer r.close()
	sessID := r.session("claude_code")

	insertEventWithPayload(t, r.h, r.bs, sessID, r.repoID, r.root, 200_000,
		"split.go", "func Solo() int { x := compute(); return x }\n")

	postLines := []string{"func Solo() int {", "x := compute()", "return x", "}"}
	eventID := r.bashEvent(sessID, 210_000)
	r.linkDelta(&toolsnap.Delta{
		Scope: "tool", Status: "complete",
		Window:   toolsnap.Window{StartedAt: 209_000, CompletedAt: 210_000, DurationMS: 1000},
		Actors:   []toolsnap.Actor{{Provider: "claude_code", SessionID: sessID, TurnID: "t1"}},
		ToolUses: []toolsnap.ToolUse{toolUse(eventID, "toolu_split", 0)},
		Files: []toolsnap.FileDelta{{
			Path: "split.go", Operation: "edit",
			BeforeHash: "a", AfterHash: "b",
			BeforeMode: "100644", AfterMode: "100644",
			Hunks: []toolsnap.Hunk{{
				OldStart: 1, OldCount: 0, NewStart: 1, NewCount: len(postLines),
				NewLines: postLines,
			}},
		}},
		Limits: toolsnap.Limits{FilesObserved: 1},
	}, []string{eventID}, 210_000)

	mustWriteFile(t, filepath.Join(r.dir, "split.go"),
		[]byte("func Solo() int {\n\tx := compute()\n\treturn x\n}\n"))
	commitHash := r.commitLinked(300_000, "split.go")

	svc := NewAttributionService()
	ctx := context.Background()

	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "0")
	v1, err := svc.AttributeCommit(ctx, AttributionInput{RepoPath: r.dir, CommitHash: commitHash})
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	if v1.AILines != 0 || v1.HumanLines != 4 {
		t.Fatalf("v1 = %d AI / %d human, want 0/4", v1.AILines, v1.HumanLines)
	}

	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "1")
	v2, err := svc.AttributeCommit(ctx, AttributionInput{RepoPath: r.dir, CommitHash: commitHash})
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	if v2.AIExactLines != 4 || v2.AIDeltaExactLines != 4 || v2.HumanLines != 0 {
		t.Fatalf("v2 = %+v, want all 4 reflowed lines exact via delta", v2)
	}
}

// Concurrent groups are excluded when per-event ownership is ambiguous.
func TestV2Regression_ConcurrentGroupExcluded(t *testing.T) {
	r := newV2Repo(t)
	defer r.close()
	claudeSess := r.session("claude_code")
	codexSess := r.session("codex")

	e1 := r.bashEvent(claudeSess, 200_000)
	e2 := r.bashEvent(codexSess, 201_000)
	r.linkDelta(&toolsnap.Delta{
		Scope: "concurrent_group", Status: "complete",
		Window: toolsnap.Window{StartedAt: 199_000, CompletedAt: 201_000, DurationMS: 2000},
		Actors: []toolsnap.Actor{
			{Provider: "claude_code", SessionID: claudeSess, TurnID: "t1"},
			{Provider: "codex", SessionID: codexSess, TurnID: "t2"},
		},
		ToolUses: []toolsnap.ToolUse{
			toolUse(e1, "toolu_c1", 0),
			toolUse(e2, "toolu_c2", 1),
		},
		Files: []toolsnap.FileDelta{{
			Path: "shared.go", Operation: "create",
			BeforeHash: "", AfterHash: "b",
			BeforeMode: "", AfterMode: "100644",
			Hunks: []toolsnap.Hunk{{
				OldStart: 1, OldCount: 0, NewStart: 1, NewCount: 1,
				NewLines: []string{"package shared"},
			}},
		}},
		Limits: toolsnap.Limits{FilesObserved: 1},
	}, []string{e1, e2}, 201_000)

	mustWriteFile(t, filepath.Join(r.dir, "shared.go"), []byte("package shared\n"))
	commitHash := r.commitLinked(300_000, "shared.go")

	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "1")
	svc := NewAttributionService()
	result, err := svc.AttributeCommit(context.Background(), AttributionInput{RepoPath: r.dir, CommitHash: commitHash})
	if err != nil {
		t.Fatalf("AttributeCommit: %v", err)
	}
	if result.AILines != 0 || result.AIProviderOnlyLines != 0 {
		t.Fatalf("AILines = %d / provider-only %d, want 0/0", result.AILines, result.AIProviderOnlyLines)
	}
	d := result.Diagnostics
	if d.DeltaGroupsEligible != 0 || d.DeltaGroupsRejected != 1 {
		t.Fatalf("delta groups = %d/%d, want 0 eligible, 1 rejected", d.DeltaGroupsEligible, d.DeltaGroupsRejected)
	}
	shared := fileByPath(t, result.Files, "shared.go")
	if shared.HumanLines != 1 || shared.Classification == "ai" {
		t.Fatalf("shared.go = %+v, want human", shared)
	}
}

// Delta fields are additive to the existing push payload.
func TestV2Regression_PushPayloadShape(t *testing.T) {
	h := testDB(t)
	ctx := context.Background()

	// Delta counters remain subsets of their quality totals.
	result := &AttributionResult{
		CommitHash:            "abc123",
		AttrVersion:           "v2",
		AIExactLines:          4,
		AIFormattedLines:      1,
		AIDeltaExactLines:     3,
		AIDeltaFormattedLines: 1,
		AILines:               5,
		TotalLines:            6,
		HumanLines:            1,
		Files: []FileAttribution{{
			Path:                  "gen.go",
			AIExactLines:          4,
			AIFormattedLines:      1,
			AIDeltaExactLines:     3,
			AIDeltaFormattedLines: 1,
			TotalLines:            6,
			EvidenceClass:         "exact",
			EvidenceClasses:       []string{"exact", "tool_delta_touch"},
		}},
	}
	payload := buildPushPayload(ctx, h, result,
		"https://github.com/o/r.git", "main", "abc123", "subject", uuid.NewString())
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)

	for _, want := range []string{
		`"attribution_version":"v2"`,
		`"ai_delta_exact_lines":3`,
		`"ai_delta_formatted_lines":1`,
		`"evidence_classes":["exact","tool_delta_touch"]`,
		// Existing required fields.
		`"remote_url":"https://github.com/o/r.git"`,
		`"commit_hash":"abc123"`,
		`"ai_lines":5`,
		`"total_lines":6`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("payload missing %s:\n%s", want, s)
		}
	}

	// Results without a version retain v1 compatibility.
	result.AttrVersion = ""
	payload = buildPushPayload(ctx, h, result,
		"https://github.com/o/r.git", "main", "abc123", "subject", uuid.NewString())
	if payload.AttrVersion != "v1" {
		t.Errorf("empty version pushed as %q, want v1", payload.AttrVersion)
	}
}

// The v1 result remains byte-stable while delta attribution is disabled.
// Regenerate with UPDATE_GOLDEN=1.
func TestV2Regression_V1GoldenResult(t *testing.T) {
	dir, commitHash := setupGoldenRepo(t)
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "0")

	svc := NewAttributionService()
	result, err := svc.AttributeCommit(context.Background(), AttributionInput{RepoPath: dir, CommitHash: commitHash})
	if err != nil {
		t.Fatalf("AttributeCommit: %v", err)
	}
	// Normalize run-specific identifiers.
	result.CommitHash = "COMMIT"
	result.CheckpointID = "CHECKPOINT"

	got, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')

	goldenPath := filepath.Join("testdata", "attribution_v1_golden.json")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		mustMkdirAll(t, "testdata")
		mustWriteFile(t, goldenPath, got)
		t.Logf("golden updated: %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("v1 result drifted from golden:\n got: %s\nwant: %s", got, want)
	}
}

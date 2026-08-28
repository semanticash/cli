package service

import (
	"context"
	"database/sql"
	"encoding/json"
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

// modCFOpts configures a modified-file carry-forward scenario.
type modCFOpts struct {
	observationBlob      string // p.go blob in the observation manifest; "" = match commit
	includeObservation   bool   // insert the workspace observation checkpoint
	anchorWindowEditPath string // path the AI edits in the anchor window (default p.go)
	anchorWindowContent  string // content of that edit (default the committed C1)
	currentEditPath      string // path the AI edits in the current window (optional)
	currentProviderTouch bool   // add a current-window provider-only (cursor) touch on p.go
	secondObservation    bool   // add a newer observation so the anchor window is empty
	malformedNewest      bool   // add a newer unlinked checkpoint with an unreadable manifest
	anchorWindowDelta    bool   // represent the anchor-window edit as a tool delta, not a line event
}

// modCFWorld identifies a prepared attribution scenario.
type modCFWorld struct {
	dir          string
	targetCommit string
}

const (
	modCFHex40a = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	modCFHex40c = "cccccccccccccccccccccccccccccccccccccccc"
	modCFHex40d = "dddddddddddddddddddddddddddddddddddddddd"
	modCFHex40e = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

// buildModCFWorld prepares an observed AI edit followed by a commit of the same
// content. Options can remove or invalidate the observation.
func buildModCFWorld(t *testing.T, opts modCFOpts) modCFWorld {
	t.Helper()
	ctx := context.Background()

	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	t.Setenv("SEMANTICA_HOME", filepath.Join(dir, ".semantica-global"))
	t.Setenv("HOME", dir)
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("config", "user.email", "test@test.com")
	git("config", "user.name", "Test")

	c0 := "package p\n"
	c1 := "package p\nfunc AI() {}\nfunc More() {}\n"
	mustWriteFile(t, filepath.Join(dir, "p.go"), []byte(c0))
	git("add", "p.go")
	git("commit", "-m", "initial")

	semDir := filepath.Join(dir, ".semantica")
	mustMkdirAll(t, semDir)
	mustWriteFile(t, filepath.Join(semDir, "enabled"), nil)
	mustMkdirAll(t, filepath.Join(semDir, "objects"))

	h, err := sqlstore.Open(ctx, filepath.Join(semDir, "lineage.db"), sqlstore.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	repo := mustOpenRepo(t, dir)
	repoRoot := repo.Root()
	repoID := uuid.NewString()
	if err := h.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{
		RepositoryID: repoID, RootPath: repoRoot, CreatedAt: 100_000, EnabledAt: 100_000,
	}); err != nil {
		t.Fatal(err)
	}
	bs, err := blobs.NewStore(filepath.Join(semDir, "objects"))
	if err != nil {
		t.Fatal(err)
	}

	// Baseline (sequence 1, cursor 0).
	insertCP(t, h, repoID, "baseline", "baseline", sql.NullString{}, 100_000)

	srcID := insertSource(t, h, repoID, "/data/session.jsonl")
	sessID := insertSessionWithProvider(t, h, repoID, srcID, uuid.NewString(), "claude_code")

	// AI edit in the anchor window.
	editPath := opts.anchorWindowEditPath
	if editPath == "" {
		editPath = "p.go"
	}
	editContent := opts.anchorWindowContent
	if editContent == "" {
		editContent = c1
	}
	if opts.anchorWindowDelta {
		insertAnchorDelta(t, h, bs, sessID, repoID, 200_000, "p.go", []string{"func AI() {}", "func More() {}"})
	} else {
		insertEventWithPayload(t, h, bs, sessID, repoID, repoRoot, 200_000, editPath, editContent)
	}

	// The shared CAS blob for p.go's committed content.
	blobHash, _, err := bs.Put(ctx, []byte(c1))
	if err != nil {
		t.Fatal(err)
	}
	obsBlob := blobHash
	if opts.observationBlob != "" {
		obsBlob = opts.observationBlob
	}

	// Workspace observation (unlinked, v2 workspace), sequence 2, cursor 1.
	if opts.includeObservation {
		obsManifest, _ := json.Marshal(blobs.Manifest{
			Version: 2, Scope: blobs.ScopeWorkspace, CreatedAt: 250_000, RepoRoot: repoRoot,
			Files: []blobs.ManifestFile{{Path: "p.go", Blob: obsBlob, Size: int64(len(c1))}},
		})
		obsHash, _, err := bs.Put(ctx, obsManifest)
		if err != nil {
			t.Fatal(err)
		}
		insertCP(t, h, repoID, uuid.NewString(), "manual", sqlstore.NullStr(obsHash), 250_000)
		// A newer observation (same content, empty window) becomes the anchor, so
		// the earlier edit falls outside the bounded anchor window.
		if opts.secondObservation {
			insertCP(t, h, repoID, uuid.NewString(), "manual", sqlstore.NullStr(obsHash), 260_000)
		}
		// A newer unlinked checkpoint whose manifest is unreadable must block
		// carry-forward rather than fall back to the valid older observation.
		if opts.malformedNewest {
			insertCP(t, h, repoID, uuid.NewString(), "manual", sqlstore.NullStr("deadbeefnotarealmanifesthash"), 260_000)
		}
	}

	// Intermediate commit-linked checkpoint (sequence 3, cursor 1): bounds the
	// current window so the anchor-window edit is not a current-window event.
	intCP := uuid.NewString()
	insertCP(t, h, repoID, intCP, "auto", sql.NullString{}, 300_000)
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: modCFHex40a, RepositoryID: repoID, CheckpointID: intCP, LinkedAt: 300_000,
	}); err != nil {
		t.Fatal(err)
	}

	// Optional current-window activity on p.go (after the intermediate checkpoint).
	if opts.currentEditPath != "" {
		insertEventWithPayload(t, h, bs, sessID, repoID, repoRoot, 350_000, opts.currentEditPath, c1)
	}
	if opts.currentProviderTouch {
		// A cursor file-edit (no line content) is a provider-only touch on p.go in
		// the current window.
		cursorSess := insertSessionWithProvider(t, h, repoID, srcID, uuid.NewString(), "cursor")
		if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
			EventID: uuid.NewString(), SessionID: cursorSess, RepositoryID: repoID, Ts: 360_000,
			Kind: "assistant", Role: sqlstore.NullStr("assistant"),
			ToolUses: sql.NullString{String: `{"content_types":["cursor_file_edit"],"tools":[{"name":"cursor_edit","file_path":"p.go","file_op":"edit"}]}`, Valid: true},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Land the commit and its checkpoint.
	mustWriteFile(t, filepath.Join(dir, "p.go"), []byte(c1))
	git("add", "p.go")
	git("commit", "-m", "land p.go")
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	targetCommit := strings.TrimSpace(string(out))

	commitManifest, _ := json.Marshal(blobs.Manifest{
		Version: 2, Scope: blobs.ScopeCommit, ObjectFormat: blobs.ObjectFormatSHA1,
		CommitHash: modCFHex40c, TreeID: modCFHex40d,
		Files: []blobs.ManifestFile{{
			Path: "p.go", Blob: blobHash, Size: int64(len(c1)),
			EntryType: blobs.EntryRegular, GitMode: "100644", GitObjectID: modCFHex40e,
		}},
	})
	commitHash, _, err := bs.Put(ctx, commitManifest)
	if err != nil {
		t.Fatal(err)
	}
	targetCP := uuid.NewString()
	insertCP(t, h, repoID, targetCP, "auto", sqlstore.NullStr(commitHash), 400_000)
	if err := h.Queries.InsertCommitLink(ctx, sqldb.InsertCommitLinkParams{
		CommitHash: targetCommit, RepositoryID: repoID, CheckpointID: targetCP, LinkedAt: 400_000,
	}); err != nil {
		t.Fatal(err)
	}
	_ = sqlstore.Close(h)
	return modCFWorld{dir: dir, targetCommit: targetCommit}
}

// insertCP inserts a completed checkpoint with an auto-allocated sequence/cursor.
func insertCP(t *testing.T, h *sqlstore.Handle, repoID, id, kind string, manifest sql.NullString, at int64) {
	t.Helper()
	if err := h.Queries.InsertCheckpoint(context.Background(), sqldb.InsertCheckpointParams{
		CheckpointID: id, RepositoryID: repoID, CreatedAt: at, Kind: kind, Status: "complete",
		ManifestHash: manifest, CompletedAt: sql.NullInt64{Int64: at, Valid: true},
	}); err != nil {
		t.Fatalf("insert checkpoint %s: %v", id, err)
	}
}

// insertAnchorDelta records a Bash event and a verified tool delta claiming the
// given added lines for a path — historical evidence with no direct line event,
// so it can only carry forward through v2 delta scoring.
func insertAnchorDelta(t *testing.T, h *sqlstore.Handle, bs *blobs.Store, sessID, repoID string, ts int64, path string, newLines []string) {
	t.Helper()
	ctx := context.Background()
	eventID := uuid.NewString()
	payload := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"gen"}}]}}`
	payloadHash, _, _ := bs.Put(ctx, []byte(payload))
	if err := h.Queries.InsertAgentEvent(ctx, sqldb.InsertAgentEventParams{
		EventID: eventID, SessionID: sessID, RepositoryID: repoID, Ts: ts,
		Kind: "assistant", Role: sqlstore.NullStr("assistant"),
		ToolUses:    sql.NullString{String: `{"content_types":["tool_use"],"tools":[{"name":"Bash"}]}`, Valid: true},
		PayloadHash: sqlstore.NullStr(payloadHash), Summary: sqlstore.NullStr("gen"),
	}); err != nil {
		t.Fatalf("insert bash event: %v", err)
	}
	delta := &toolsnap.Delta{
		Scope: "tool", Status: "complete",
		Window: toolsnap.Window{StartedAt: ts - 1000, CompletedAt: ts, DurationMS: 1000},
		Actors: []toolsnap.Actor{{Provider: "claude_code", SessionID: sessID, TurnID: "t1"}},
		ToolUses: []toolsnap.ToolUse{{
			ToolUseID: "toolu_" + path, ToolName: "Bash", EventID: eventID, Actor: 0,
		}},
		Files: []toolsnap.FileDelta{{
			Path: path, Operation: "edit", BeforeHash: "a", AfterHash: "b",
			BeforeMode: "100644", AfterMode: "100644",
			Hunks: []toolsnap.Hunk{{OldStart: 1, OldCount: 0, NewStart: 2, NewCount: len(newLines), NewLines: newLines}},
		}},
		Limits: toolsnap.Limits{FilesObserved: 1},
	}
	raw, err := delta.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	deltaHash, _, err := bs.Put(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Queries.InsertEvidenceLinkIfAbsent(ctx, sqldb.InsertEvidenceLinkIfAbsentParams{
		EventID: eventID, EvidenceKind: "tool_delta", EvidenceHash: deltaHash, GroupID: uuid.NewString(), CreatedAt: ts,
	}); err != nil {
		t.Fatal(err)
	}
}

func hasClass(f FileAttribution, class string) bool {
	for _, c := range f.EvidenceClasses {
		if c == class {
			return true
		}
	}
	return false
}

func hasProvider(f FileAttribution, provider string) bool {
	for _, p := range f.Providers {
		if p == provider {
			return true
		}
	}
	return false
}

// A modified file whose content matches an unlinked workspace observation carries
// the observation window's AI line evidence forward, tagged carry_forward and
// credited to the historical provider.
func TestModifiedCF_E2E_CarriesForwardWithClassAndProvider(t *testing.T) {
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "0")
	w := buildModCFWorld(t, modCFOpts{includeObservation: true})

	result, err := NewAttributionService().AttributeCommit(context.Background(), AttributionInput{RepoPath: w.dir, CommitHash: w.targetCommit})
	if err != nil {
		t.Fatalf("AttributeCommit: %v", err)
	}
	p := fileByPath(t, result.Files, "p.go")
	if p.AILines < 1 {
		t.Fatalf("p.go AILines = %d, want >= 1 (carried forward)", p.AILines)
	}
	if !hasClass(p, string(attrreporting.EvidenceCarryForward)) {
		t.Errorf("p.go evidence classes = %v, want carry_forward", p.EvidenceClasses)
	}
	if !hasProvider(p, "claude_code") {
		t.Errorf("p.go providers = %v, want claude_code", p.Providers)
	}
}

// Without a workspace observation, no evidence carries forward.
func TestModifiedCF_E2E_NoObservationNoCarry(t *testing.T) {
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "0")
	w := buildModCFWorld(t, modCFOpts{includeObservation: false})

	result, err := NewAttributionService().AttributeCommit(context.Background(), AttributionInput{RepoPath: w.dir, CommitHash: w.targetCommit})
	if err != nil {
		t.Fatalf("AttributeCommit: %v", err)
	}
	p := fileByPath(t, result.Files, "p.go")
	if p.AILines != 0 {
		t.Fatalf("p.go AILines = %d, want 0 (no anchor)", p.AILines)
	}
}

// Mismatched observed and committed content blocks carry-forward.
func TestModifiedCF_E2E_InvalidAnchorNoCarry(t *testing.T) {
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "0")
	w := buildModCFWorld(t, modCFOpts{
		includeObservation: true,
		observationBlob:    strings.Repeat("b", 64), // valid CAS hash, but != commit blob
	})
	result, err := NewAttributionService().AttributeCommit(context.Background(), AttributionInput{RepoPath: w.dir, CommitHash: w.targetCommit})
	if err != nil {
		t.Fatalf("AttributeCommit: %v", err)
	}
	if p := fileByPath(t, result.Files, "p.go"); p.AILines != 0 {
		t.Fatalf("p.go AILines = %d, want 0 (invalid anchor)", p.AILines)
	}
}

// Current-window line evidence prevents carry-forward.
func TestModifiedCF_E2E_CurrentEvidenceWins(t *testing.T) {
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "0")
	w := buildModCFWorld(t, modCFOpts{includeObservation: true, currentEditPath: "p.go"})
	result, err := NewAttributionService().AttributeCommit(context.Background(), AttributionInput{RepoPath: w.dir, CommitHash: w.targetCommit})
	if err != nil {
		t.Fatalf("AttributeCommit: %v", err)
	}
	p := fileByPath(t, result.Files, "p.go")
	if p.AILines < 1 {
		t.Fatalf("p.go AILines = %d, want >= 1 (current evidence)", p.AILines)
	}
	if hasClass(p, string(attrreporting.EvidenceCarryForward)) {
		t.Errorf("p.go tagged carry_forward, but current evidence should win: %v", p.EvidenceClasses)
	}
}

// AI activity on an unrelated file in the anchor window does not increase
// attribution for the committed path.
func TestModifiedCF_E2E_UnrelatedActivityNoIncrease(t *testing.T) {
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "0")
	w := buildModCFWorld(t, modCFOpts{includeObservation: true, anchorWindowEditPath: "q.go"})
	result, err := NewAttributionService().AttributeCommit(context.Background(), AttributionInput{RepoPath: w.dir, CommitHash: w.targetCommit})
	if err != nil {
		t.Fatalf("AttributeCommit: %v", err)
	}
	if p := fileByPath(t, result.Files, "p.go"); p.AILines != 0 {
		t.Fatalf("p.go AILines = %d, want 0 (unrelated activity)", p.AILines)
	}
}

// An edit that lands only in an earlier observation's window is outside the
// newest anchor's bounded window and does not carry.
func TestModifiedCF_E2E_BoundedWindowOnly(t *testing.T) {
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "0")
	w := buildModCFWorld(t, modCFOpts{includeObservation: true, secondObservation: true})
	result, err := NewAttributionService().AttributeCommit(context.Background(), AttributionInput{RepoPath: w.dir, CommitHash: w.targetCommit})
	if err != nil {
		t.Fatalf("AttributeCommit: %v", err)
	}
	if p := fileByPath(t, result.Files, "p.go"); p.AILines != 0 {
		t.Fatalf("p.go AILines = %d, want 0 (edit outside the anchor window)", p.AILines)
	}
}

// Under default v2, a historical edit represented only by a tool delta (no line
// event) carries forward.
func TestModifiedCF_E2E_V2DeltaCarriesForward(t *testing.T) {
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "1")
	w := buildModCFWorld(t, modCFOpts{includeObservation: true, anchorWindowDelta: true})
	result, err := NewAttributionService().AttributeCommit(context.Background(), AttributionInput{RepoPath: w.dir, CommitHash: w.targetCommit})
	if err != nil {
		t.Fatalf("AttributeCommit: %v", err)
	}
	p := fileByPath(t, result.Files, "p.go")
	if p.AILines < 1 || !hasClass(p, string(attrreporting.EvidenceCarryForward)) {
		t.Fatalf("p.go = %+v, want delta-backed carry forward", p)
	}
	if p.AIDeltaExactLines < 1 {
		t.Errorf("p.go delta-exact lines = %d, want the historical delta credited", p.AIDeltaExactLines)
	}
}

// Under v1, a delta-only historical edit has no line evidence and does not carry.
func TestModifiedCF_E2E_V1DeltaOnlyDoesNotCarry(t *testing.T) {
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "0")
	w := buildModCFWorld(t, modCFOpts{includeObservation: true, anchorWindowDelta: true})
	result, err := NewAttributionService().AttributeCommit(context.Background(), AttributionInput{RepoPath: w.dir, CommitHash: w.targetCommit})
	if err != nil {
		t.Fatalf("AttributeCommit: %v", err)
	}
	if p := fileByPath(t, result.Files, "p.go"); p.AILines != 0 {
		t.Fatalf("p.go AILines = %d, want 0 (v1 ignores the historical delta)", p.AILines)
	}
}

// A newer unlinked observation with an unreadable manifest blocks carry-forward
// rather than anchoring on the valid older observation.
func TestModifiedCF_E2E_MalformedNewestBlocks(t *testing.T) {
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "0")
	w := buildModCFWorld(t, modCFOpts{includeObservation: true, malformedNewest: true})
	result, err := NewAttributionService().AttributeCommit(context.Background(), AttributionInput{RepoPath: w.dir, CommitHash: w.targetCommit})
	if err != nil {
		t.Fatalf("AttributeCommit: %v", err)
	}
	if p := fileByPath(t, result.Files, "p.go"); p.AILines != 0 {
		t.Fatalf("p.go AILines = %d, want 0 (malformed newest observation blocks)", p.AILines)
	}
}

// Line attribution subsumes provider-only reporting for the same file.
func TestModifiedCF_E2E_CurrentProviderOnlySubsumed(t *testing.T) {
	t.Setenv("SEMANTICA_ATTRIBUTION_V2", "0")
	ctx := context.Background()

	// Same current cursor provider-only touch, no anchor: p.go is provider-only,
	// confirming the touch is real and reportable on its own.
	noAnchor := buildModCFWorld(t, modCFOpts{includeObservation: false, currentProviderTouch: true})
	rA, err := NewAttributionService().AttributeCommit(ctx, AttributionInput{RepoPath: noAnchor.dir, CommitHash: noAnchor.targetCommit})
	if err != nil {
		t.Fatalf("AttributeCommit (no anchor): %v", err)
	}
	pA := fileByPath(t, rA.Files, "p.go")
	if pA.AILines != 0 || pA.AIProviderOnlyLines < 1 || !hasProvider(pA, "cursor") {
		t.Fatalf("no-anchor p.go = %+v, want cursor provider-only", pA)
	}

	// With the anchor, the file carries to the historical provider, is not human,
	// and the current provider-only touch is subsumed (cursor no longer reported).
	w := buildModCFWorld(t, modCFOpts{includeObservation: true, currentProviderTouch: true})
	rB, err := NewAttributionService().AttributeCommit(ctx, AttributionInput{RepoPath: w.dir, CommitHash: w.targetCommit})
	if err != nil {
		t.Fatalf("AttributeCommit (anchor): %v", err)
	}
	pB := fileByPath(t, rB.Files, "p.go")
	if pB.AILines < 1 || !hasClass(pB, string(attrreporting.EvidenceCarryForward)) {
		t.Fatalf("anchored p.go = %+v, want carried line evidence", pB)
	}
	if !hasProvider(pB, "claude_code") {
		t.Errorf("anchored p.go providers = %v, want carried claude_code", pB.Providers)
	}
	if hasProvider(pB, "cursor") {
		t.Errorf("anchored p.go providers = %v, cursor should be subsumed by line evidence", pB.Providers)
	}
}

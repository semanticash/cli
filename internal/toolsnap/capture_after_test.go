package toolsnap

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCaptureAfterEmptyDelta(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	pre, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("pre: %v", err)
	}
	res, err := s.CaptureAfter(ctx, pre)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if res.Post.TreeHash != pre.TreeHash || res.Files != nil || res.BytesRead != 0 || res.Truncated {
		t.Errorf("empty delta = %+v", res)
	}
}

func TestCaptureAfterComputesHunks(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	pre, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("pre: %v", err)
	}
	writeFile(t, root, "a.txt", "alpha\nsecond line\n")
	writeFile(t, root, "gen/out.txt", "generated\n")
	if err := os.Remove(filepath.Join(root, "sub", "b.txt")); err != nil {
		t.Fatal(err)
	}

	res, err := s.CaptureAfter(ctx, pre)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	files, bytesRead := res.Files, res.BytesRead
	if len(files) != 3 {
		t.Fatalf("files = %+v", files)
	}
	byPath := map[string]FileDelta{}
	for _, f := range files {
		byPath[f.Path] = f
	}

	edit := byPath["a.txt"]
	if edit.Operation != "edit" || len(edit.Hunks) != 1 {
		t.Fatalf("a.txt = %+v", edit)
	}
	h := edit.Hunks[0]
	if h.OldStart != 2 || h.OldCount != 0 || h.NewCount != 1 || h.NewLines[0] != "second line" {
		t.Errorf("a.txt hunk = %+v", h)
	}

	created := byPath["gen/out.txt"]
	if created.Operation != "create" || len(created.Hunks) != 1 || created.Hunks[0].NewLines[0] != "generated" {
		t.Errorf("gen/out.txt = %+v", created)
	}
	deleted := byPath["sub/b.txt"]
	if deleted.Operation != "delete" || len(deleted.Hunks) != 1 || deleted.Hunks[0].OldLines[0] != "beta" {
		t.Errorf("sub/b.txt = %+v", deleted)
	}
	if bytesRead == 0 {
		t.Error("bytesRead not accounted")
	}
}

func TestCaptureAfterBinaryFileTouchOnly(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	pre, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("pre: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := s.CaptureAfter(ctx, pre)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if len(res.Files) != 1 || !res.Files[0].Binary || res.Files[0].Hunks != nil {
		t.Errorf("binary delta = %+v", res.Files)
	}
}

func TestCaptureAfterHeadMovedFailsPartial(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	pre, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("pre: %v", err)
	}
	writeFile(t, root, "a.txt", "committed during window\n")
	run(t, root, "git", "add", "a.txt")
	run(t, root, "git", "commit", "-q", "-m", "window commit")

	_, err = s.CaptureAfter(ctx, pre)
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Reason != ReasonHeadChanged {
		t.Fatalf("err = %v, want PartialError %s", err, ReasonHeadChanged)
	}
}

func TestCaptureAfterByteLimitFailsPartial(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	pre, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("pre: %v", err)
	}
	s.MaxBytesRead = 8
	writeFile(t, root, "big.txt", "well beyond eight bytes of content\n")
	_, err = s.CaptureAfter(ctx, pre)
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Reason != ReasonByteLimit {
		t.Fatalf("err = %v, want PartialError %s", err, ReasonByteLimit)
	}
}

// TestSubmoduleLifecycleIsTouchOnlyEvidence covers gitlink changes.
func TestSubmoduleLifecycleIsTouchOnlyEvidence(t *testing.T) {
	// Set identity for commits made in the cloned submodule.
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")

	sub := t.TempDir()
	run(t, sub, "git", "init", "-q", "-b", "main")
	run(t, sub, "git", "config", "user.email", "t@example.com")
	run(t, sub, "git", "config", "user.name", "t")
	writeFile(t, sub, "inner.txt", "inner\n")
	run(t, sub, "git", "add", ".")
	run(t, sub, "git", "commit", "-q", "-m", "sub init")

	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	// Add.
	pre, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("pre add: %v", err)
	}
	run(t, root, "git", "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "vendor/dep")
	res, err := s.CaptureAfter(ctx, pre)
	if err != nil {
		t.Fatalf("after add: %v", err)
	}
	var link *FileDelta
	for i := range res.Files {
		if res.Files[i].Path == "vendor/dep" {
			link = &res.Files[i]
		}
	}
	if link == nil || link.AfterMode != "160000" || link.Hunks != nil {
		t.Fatalf("submodule add delta = %+v", res.Files)
	}

	// Commit the add so update starts from a clean tree.
	run(t, root, "git", "commit", "-q", "-m", "add submodule")
	s = openTestStore(t, root) // refresh cached HEAD

	// Update: advance the subrepository and stage the new pointer.
	pre, err = s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("pre update: %v", err)
	}
	subWT := filepath.Join(root, "vendor", "dep")
	writeFile(t, root, "vendor/dep/inner.txt", "inner v2\n")
	run(t, subWT, "git", "commit", "-aqm", "advance")
	res, err = s.CaptureAfter(ctx, pre)
	if err != nil {
		t.Fatalf("after update: %v", err)
	}
	files := res.Files
	if len(files) != 1 || files[0].Path != "vendor/dep" ||
		files[0].BeforeMode != "160000" || files[0].AfterMode != "160000" ||
		files[0].Hunks != nil || files[0].BeforeHash == files[0].AfterHash {
		t.Fatalf("submodule update delta = %+v", files)
	}

	// Delete.
	pre, err = s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("pre delete: %v", err)
	}
	run(t, root, "git", "rm", "-qf", "vendor/dep")
	res, err = s.CaptureAfter(ctx, pre)
	if err != nil {
		t.Fatalf("after delete: %v", err)
	}
	byPath := map[string]FileDelta{}
	for _, f := range res.Files {
		byPath[f.Path] = f
	}
	del, ok := byPath["vendor/dep"]
	if !ok || del.Operation != "delete" || del.BeforeMode != "160000" || del.Hunks != nil {
		t.Fatalf("submodule delete delta = %+v", res.Files)
	}
}

// TestDiffBudgetExhaustionIsDeterministic verifies stable fallback.
func TestDiffBudgetExhaustionIsDeterministic(t *testing.T) {
	oldContent := []byte("a\nb\nc\nd\ne\n")
	newContent := []byte("a\nB\nc\nD\ne\n")
	first := func() ([]Hunk, []Hunk) {
		// One fine-grained diff consumes the full pre-split budget.
		budget := int64(40)
		h1, err := diffLinesBudget(context.Background(), oldContent, newContent, &budget)
		if err != nil {
			t.Fatal(err)
		}
		h2, err := diffLinesBudget(context.Background(), oldContent, newContent, &budget)
		if err != nil {
			t.Fatal(err)
		}
		return h1, h2
	}
	a1, a2 := first()
	b1, b2 := first()
	if !reflect.DeepEqual(a1, b1) || !reflect.DeepEqual(a2, b2) {
		t.Fatal("budgeted diffing not deterministic across runs")
	}
	// Exhausted work produces one coarse hunk.
	if len(a2) != 1 || a2[0].OldCount != 3 || a2[0].NewCount != 3 {
		t.Errorf("exhausted diff = %+v, want coarse hunk over trimmed region", a2)
	}
	// The first diff remains precise.
	if len(a1) != 2 {
		t.Errorf("budgeted first diff = %+v, want two hunks", a1)
	}
}

func TestDeltaValidateRejectsMalformed(t *testing.T) {
	base := func() *Delta {
		return &Delta{
			Scope: "tool", Status: "complete",
			Actors:   []Actor{{Provider: "p", SessionID: "s", TurnID: "t"}},
			ToolUses: []ToolUse{{ToolUseID: "tu", EventID: "e", Actor: 0}},
			Files:    []FileDelta{{Path: "a.txt", Operation: "edit"}},
		}
	}
	if _, err := base().CanonicalBytes(); err != nil {
		t.Fatalf("valid delta rejected: %v", err)
	}

	d := base()
	d.ToolUses[0].Actor = 2
	if _, err := d.CanonicalBytes(); err == nil {
		t.Error("out-of-range actor index accepted")
	}
	d = base()
	d.Actors = append(d.Actors, d.Actors[0])
	if _, err := d.CanonicalBytes(); err == nil {
		t.Error("duplicate actor identity accepted")
	}
	d = base()
	d.ToolUses = append(d.ToolUses, d.ToolUses[0])
	if _, err := d.CanonicalBytes(); err == nil {
		t.Error("duplicate tool use accepted")
	}
	d = base()
	d.Files = append(d.Files, d.Files[0])
	if _, err := d.CanonicalBytes(); err == nil {
		t.Error("duplicate file path accepted")
	}
	d = base()
	d.Files[0].Hunks = []Hunk{{OldCount: 2, OldLines: []string{"only one"}}}
	if _, err := d.CanonicalBytes(); err == nil {
		t.Error("hunk count disagreement accepted")
	}

	// Canonical evidence uses portable repository-relative paths.
	for _, bad := range []string{
		"", "/abs/path.go", `dir\win.go`, "C:/drive.go", "c:relative.go",
		"a/../victim.go", "./dotted.go", "..", "a/./b.go", "a//b.go", "a/b/",
		"a\x00b",
	} {
		d = base()
		d.Files[0].Path = bad
		if _, err := d.CanonicalBytes(); err == nil {
			t.Errorf("non-canonical path %q accepted", bad)
		}
	}
	for _, good := range []string{"a.txt", "pkg/sub/file.go", "weird name.go", "...go"} {
		d = base()
		d.Files[0].Path = good
		if _, err := d.CanonicalBytes(); err != nil {
			t.Errorf("canonical path %q rejected: %v", good, err)
		}
	}
}

func TestCanonicalBytesNilAndEmptyCollectionsAgree(t *testing.T) {
	withNil := &Delta{Scope: "tool", Status: "complete"}
	withEmpty := &Delta{
		Scope: "tool", Status: "complete",
		Actors: []Actor{}, ToolUses: []ToolUse{}, Files: []FileDelta{},
	}
	a, err := withNil.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	b, err := withEmpty.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Errorf("nil and empty collections serialize differently:\n%s\n%s", a, b)
	}
}

func TestCanonicalBytesShuffledInputsOneHash(t *testing.T) {
	build := func(actorOrder []int, fileOrder []int) *Delta {
		actors := []Actor{
			{Provider: "claude_code", SessionID: "s2", TurnID: "t1"},
			{Provider: "claude_code", SessionID: "s1", TurnID: "t1"},
		}
		files := []FileDelta{
			{Path: "b.txt", Operation: "edit", Hunks: []Hunk{
				{OldStart: 9, NewStart: 9, OldCount: 1, NewCount: 1, OldLines: []string{"x"}, NewLines: []string{"y"}},
				{OldStart: 2, NewStart: 2, OldCount: 1, NewCount: 1, OldLines: []string{"p"}, NewLines: []string{"q"}},
			}},
			{Path: "a.txt", Operation: "create"},
		}
		d := &Delta{
			Scope:  "concurrent_group",
			Status: "partial",
			Reason: "ambiguous_concurrency",
			Window: Window{StartedAt: 100, CompletedAt: 200, DurationMS: 100},
		}
		for _, i := range actorOrder {
			d.Actors = append(d.Actors, actors[i])
		}
		// Tool uses reference the pre-shuffle actor positions.
		for n, i := range actorOrder {
			d.ToolUses = append(d.ToolUses, ToolUse{
				ToolUseID: "tu" + actors[i].SessionID, ToolName: "Bash",
				EventID: "e" + actors[i].SessionID, Actor: n,
			})
		}
		for _, i := range fileOrder {
			d.Files = append(d.Files, files[i])
		}
		return d
	}

	first, err := build([]int{0, 1}, []int{0, 1}).CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	second, err := build([]int{1, 0}, []int{1, 0}).CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(first) != sha256.Sum256(second) {
		t.Fatalf("shuffled assembly produced different canonical bytes:\n%s\n%s", first, second)
	}

	// The normalized form must keep tool uses pointing at their actors.
	d, err := ParseDelta(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, tu := range d.ToolUses {
		if want := "tu" + d.Actors[tu.Actor].SessionID; tu.ToolUseID != want {
			t.Errorf("tool use %s references actor %d (%s)", tu.ToolUseID, tu.Actor, d.Actors[tu.Actor].SessionID)
		}
	}
}

// TestNewlineDenseFileDegradesToTruncatedTouch covers the line bound.
func TestNewlineDenseFileDegradesToTruncatedTouch(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	ctx := context.Background()

	pre, err := s.CaptureBefore(ctx)
	if err != nil {
		t.Fatalf("pre: %v", err)
	}
	dense := strings.Repeat("\n", maxDiffLinesPerFile+2)
	writeFile(t, root, "dense.txt", dense)
	res, err := s.CaptureAfter(ctx, pre)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].Hunks != nil || res.Files[0].Binary {
		t.Fatalf("dense delta = %+v", res.Files)
	}
	if !res.Files[0].Truncated {
		t.Error("truncation not attributed to the file")
	}
	if !res.Truncated {
		t.Error("truncation not reported in aggregate")
	}
}

// countingCtx cancels after a fixed number of polls.
type countingCtx struct {
	context.Context
	calls     int
	failAfter int
}

func (c *countingCtx) Err() error {
	c.calls++
	if c.calls > c.failAfter {
		return context.Canceled
	}
	return nil
}

// TestDifferHonorsCancellationMidSearch cancels after work begins.
func TestDifferHonorsCancellationMidSearch(t *testing.T) {
	var oldB, newB strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&oldB, "old-%d\n", i)
		fmt.Fprintf(&newB, "new-%d\n", i)
	}
	// Cancel during a later Myers depth.
	ctx := &countingCtx{Context: context.Background(), failAfter: 5}
	_, err := diffLinesBudget(ctx, []byte(oldB.String()), []byte(newB.String()), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled from inside the search", err)
	}
	if ctx.calls <= 5 {
		t.Fatalf("Err consulted %d times; cancellation did not land mid-search", ctx.calls)
	}
}

func TestTruncationInvariantBothDirections(t *testing.T) {
	// Per-file truncation requires the aggregate flag.
	d := &Delta{Scope: "tool", Status: "complete",
		Files: []FileDelta{{Path: "a.txt", Operation: "edit", Truncated: true}}}
	if _, err := d.CanonicalBytes(); err == nil {
		t.Error("hidden truncation accepted: file truncated, limits not")
	}
	// The aggregate flag requires a truncated file.
	d = &Delta{Scope: "tool", Status: "complete",
		Limits: Limits{Truncated: true},
		Files:  []FileDelta{{Path: "a.txt", Operation: "edit"}}}
	if _, err := d.CanonicalBytes(); err == nil {
		t.Error("phantom truncation accepted: limits truncated, no file is")
	}
	// Matching flags remain canonical.
	d = &Delta{Scope: "tool", Status: "complete",
		Limits: Limits{Truncated: true},
		Files:  []FileDelta{{Path: "a.txt", Operation: "edit", Truncated: true}}}
	raw, err := d.CanonicalBytes()
	if err != nil {
		t.Fatalf("agreeing truncation rejected: %v", err)
	}
	if _, err := ParseDelta(raw); err != nil {
		t.Fatalf("agreeing truncation blob rejected at parse: %v", err)
	}
}

func TestGeometryRejectsAdjacentHunksAndDegenerates(t *testing.T) {
	base := func(hunks []Hunk) *Delta {
		return &Delta{Scope: "tool", Status: "complete",
			Files: []FileDelta{{Path: "a.txt", Operation: "edit", Hunks: hunks}}}
	}
	// Adjacent hunks could encode one change twice.
	d := base([]Hunk{
		{OldStart: 3, NewStart: 3, OldCount: 1, NewCount: 1, OldLines: []string{"a"}, NewLines: []string{"b"}},
		{OldStart: 4, NewStart: 4, OldCount: 1, NewCount: 1, OldLines: []string{"c"}, NewLines: []string{"d"}},
	})
	if _, err := d.CanonicalBytes(); err == nil {
		t.Error("adjacent hunks accepted")
	}
	d = base([]Hunk{{OldStart: 0, NewStart: 1, OldCount: 1, NewCount: 1, OldLines: []string{"a"}, NewLines: []string{"b"}}})
	if _, err := d.CanonicalBytes(); err == nil {
		t.Error("hunk start below 1 accepted")
	}
	d = base([]Hunk{{OldStart: 2, NewStart: 2, OldLines: []string{}, NewLines: []string{}}})
	if _, err := d.CanonicalBytes(); err == nil {
		t.Error("hunk changing nothing accepted")
	}
	// Consecutive changes produce one hunk.
	if hunks := diffLines([]byte("a\nb\nc\nd\n"), []byte("a\nB\nC\nd\n")); len(hunks) != 1 {
		t.Errorf("consecutive change split into %d hunks", len(hunks))
	}
}

func TestParseDeltaRejectsNonCanonicalRepresentation(t *testing.T) {
	d := &Delta{Scope: "concurrent_group", Status: "partial", Reason: "ambiguous_concurrency",
		Actors: []Actor{
			{Provider: "p", SessionID: "s1", TurnID: "t"},
			{Provider: "p", SessionID: "s2", TurnID: "t"},
		},
		ToolUses: []ToolUse{
			{ToolUseID: "tu1", EventID: "e1", Actor: 0},
			{ToolUseID: "tu2", EventID: "e2", Actor: 1},
		}}
	raw, err := d.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDelta(raw); err != nil {
		t.Fatalf("canonical blob rejected: %v", err)
	}
	// Reject non-canonical actor order.
	shuffled := strings.Replace(string(raw), `"s1"`, `"sX"`, 1)
	shuffled = strings.Replace(shuffled, `"s2"`, `"s1"`, 1)
	shuffled = strings.Replace(shuffled, `"sX"`, `"s2"`, 1)
	if _, err := ParseDelta([]byte(shuffled)); err == nil {
		t.Error("shuffled actor order accepted as canonical")
	}
	// Reject null canonical collections.
	nulls := strings.Replace(string(raw), `"files":[]`, `"files":null`, 1)
	if _, err := ParseDelta([]byte(nulls)); err == nil {
		t.Error("null collection accepted as canonical")
	}
}

func TestCaptureAfterExpiredDeadlineIsTimeoutPartial(t *testing.T) {
	root := testRepo(t)
	s := openTestStore(t, root)
	pre, err := s.CaptureBefore(context.Background())
	if err != nil {
		t.Fatalf("pre: %v", err)
	}
	writeFile(t, root, "a.txt", "change under expired deadline\n")
	ctx, cancel := context.WithTimeout(context.Background(), 1)
	defer cancel()
	<-ctx.Done()

	_, err = s.CaptureAfter(ctx, pre)
	var pe *PartialError
	if !errors.As(err, &pe) || pe.Reason != ReasonTimeout {
		t.Fatalf("err = %v, want PartialError %s", err, ReasonTimeout)
	}
}

func TestDeltaCoherenceRules(t *testing.T) {
	base := func() *Delta {
		return &Delta{Scope: "tool", Status: "complete",
			Files: []FileDelta{{Path: "a.txt", Operation: "edit"}}}
	}
	d := base()
	d.Files[0].Binary = true
	d.Files[0].Hunks = []Hunk{{OldCount: 0, NewCount: 1, NewLines: []string{"x"}}}
	if _, err := d.CanonicalBytes(); err == nil {
		t.Error("binary file with hunks accepted")
	}
	d = base()
	d.Files[0].Operation = "renamed"
	if _, err := d.CanonicalBytes(); err == nil {
		t.Error("unknown operation accepted")
	}
	d = base()
	d.Status = "partial"
	if _, err := d.CanonicalBytes(); err == nil {
		t.Error("partial without reason accepted")
	}
	d = base()
	d.Reason = "leftover"
	if _, err := d.CanonicalBytes(); err == nil {
		t.Error("complete with reason accepted")
	}
	d = base()
	d.Files[0].Hunks = []Hunk{
		{OldStart: 3, NewStart: 3, OldCount: 2, NewCount: 2, OldLines: []string{"a", "b"}, NewLines: []string{"c", "d"}},
		{OldStart: 4, NewStart: 9, OldCount: 1, NewCount: 1, OldLines: []string{"e"}, NewLines: []string{"f"}},
	}
	if _, err := d.CanonicalBytes(); err == nil {
		t.Error("overlapping hunks accepted")
	}
	d = base()
	d.Files[0].Hunks = []Hunk{
		{OldStart: 3, NewStart: 3, OldCount: 1, NewCount: 1, OldLines: []string{"a"}, NewLines: []string{"b"}},
		{OldStart: 3, NewStart: 3, OldCount: 1, NewCount: 1, OldLines: []string{"a"}, NewLines: []string{"b"}},
	}
	if _, err := d.CanonicalBytes(); err == nil {
		t.Error("duplicate hunks accepted")
	}
}

func TestParseDeltaStrictness(t *testing.T) {
	valid := &Delta{Scope: "tool", Status: "complete"}
	raw, err := valid.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDelta(raw); err != nil {
		t.Fatalf("valid delta rejected: %v", err)
	}
	if _, err := ParseDelta(append(raw, []byte(`{"extra":1}`)...)); err == nil {
		t.Error("trailing data accepted")
	}
	unknownField := strings.Replace(string(raw), `"version"`, `"surprise":true,"version"`, 1)
	if _, err := ParseDelta([]byte(unknownField)); err == nil {
		t.Error("unknown field accepted")
	}
	// Reject well-formed JSON with an invalid actor reference.
	badActor := `{"version":1,"kind":"agent_tool_delta","scope":"tool","status":"complete",` +
		`"window":{"started_at":0,"completed_at":0,"duration_ms":0},` +
		`"actors":[],"tool_uses":[{"tool_use_id":"tu","tool_name":"Bash","event_id":"e","actor":3}],` +
		`"files":[],"limits":{"files_observed":0,"bytes_read":0,"truncated":false}}`
	if _, err := ParseDelta([]byte(badActor)); err == nil {
		t.Error("out-of-range actor reference accepted at parse time")
	}
}

func TestParseDeltaRejectsUnknownShapes(t *testing.T) {
	valid := &Delta{Scope: "tool", Status: "complete"}
	raw, err := valid.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDelta(raw); err != nil {
		t.Fatalf("valid delta rejected: %v", err)
	}
	for _, bad := range []string{
		`{"version":2,"kind":"agent_tool_delta","scope":"tool","status":"complete"}`,
		`{"version":1,"kind":"other","scope":"tool","status":"complete"}`,
		`{"version":1,"kind":"agent_tool_delta","scope":"weird","status":"complete"}`,
		`{"version":1,"kind":"agent_tool_delta","scope":"tool","status":"maybe"}`,
		`not json`,
	} {
		if _, err := ParseDelta([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

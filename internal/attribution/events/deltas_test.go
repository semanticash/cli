package events

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"testing"

	"github.com/semanticash/cli/internal/toolsnap"
)

// memBlobs records requested hashes.
type memBlobs struct {
	blobs map[string][]byte
	gets  []string
}

func (m *memBlobs) Get(_ context.Context, hash string) ([]byte, error) {
	m.gets = append(m.gets, hash)
	b, ok := m.blobs[hash]
	if !ok {
		return nil, errors.New("missing blob")
	}
	return b, nil
}

func (m *memBlobs) put(raw []byte) string {
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	if m.blobs == nil {
		m.blobs = map[string][]byte{}
	}
	m.blobs[hash] = raw
	return hash
}

func baseDelta(eventID string, files []toolsnap.FileDelta) *toolsnap.Delta {
	return &toolsnap.Delta{
		Scope: "tool", Status: "complete",
		Window: toolsnap.Window{StartedAt: 100, CompletedAt: 200, DurationMS: 100},
		Actors: []toolsnap.Actor{{Provider: "claude_code", SessionID: "s1", TurnID: "t1"}},
		ToolUses: []toolsnap.ToolUse{{
			ToolUseID: "toolu_1", ToolName: "Bash", EventID: eventID, Actor: 0,
		}},
		Files:  files,
		Limits: toolsnap.Limits{FilesObserved: len(files)},
	}
}

func editFile(path string, newLines ...string) toolsnap.FileDelta {
	return toolsnap.FileDelta{
		Path: path, Operation: "edit",
		BeforeHash: "a", AfterHash: "b",
		BeforeMode: "100644", AfterMode: "100644",
		Hunks: []toolsnap.Hunk{{
			OldStart: 1, OldCount: 0, NewStart: 1, NewCount: len(newLines),
			NewLines: newLines,
		}},
	}
}

func putDelta(t *testing.T, m *memBlobs, d *toolsnap.Delta) string {
	t.Helper()
	raw, err := d.CanonicalBytes()
	if err != nil {
		t.Fatal(err)
	}
	return m.put(raw)
}

func link(eventID, hash, group string) DeltaLink {
	return DeltaLink{EventID: eventID, EvidenceHash: hash, GroupID: group, Provider: "claude_code"}
}

func build(t *testing.T, m *memBlobs, links []DeltaLink) *DeltaCandidates {
	t.Helper()
	out, err := BuildDeltaCandidates(context.Background(), links, m)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBuildDeltaCandidates_OrderedClaims(t *testing.T) {
	m := &memBlobs{}
	hash := putDelta(t, m, baseDelta("evt-1", []toolsnap.FileDelta{
		editFile("pkg/a.go", "return nil", "x := 1", "return nil"),
	}))
	out := build(t, m, []DeltaLink{link("evt-1", hash, "g1")})
	if out.Diags.GroupsEligible != 1 || len(out.Diags.Rejected) != 0 {
		t.Fatalf("diags = %+v", out.Diags)
	}
	want := []DeltaClaim{
		{Line: "return nil", Provider: "claude_code", GroupID: "g1"},
		{Line: "x := 1", Provider: "claude_code", GroupID: "g1"},
		{Line: "return nil", Provider: "claude_code", GroupID: "g1"},
	}
	if !reflect.DeepEqual(out.Claims["pkg/a.go"], want) {
		t.Fatalf("claims = %+v", out.Claims["pkg/a.go"])
	}
}

// Groups load once and retain event order.
func TestBuildDeltaCandidates_DedupAndCaptureOrder(t *testing.T) {
	m := &memBlobs{}
	h1 := putDelta(t, m, baseDelta("evt-1", []toolsnap.FileDelta{editFile("f.go", "first")}))
	d2 := baseDelta("evt-2", []toolsnap.FileDelta{editFile("f.go", "second")})
	d2.ToolUses[0].ToolUseID = "toolu_2"
	h2 := putDelta(t, m, d2)
	out := build(t, m, []DeltaLink{
		link("evt-1", h1, "g1"),
		link("evt-1", h1, "g1"), // duplicate link
		link("evt-2", h2, "g2"),
	})
	if len(m.gets) != 2 {
		t.Fatalf("blob loads = %v, want one per group", m.gets)
	}
	lines := []string{}
	for _, c := range out.Claims["f.go"] {
		lines = append(lines, c.Line)
	}
	if !reflect.DeepEqual(lines, []string{"first", "second"}) {
		t.Fatalf("claims = %v, want capture order preserved", lines)
	}
}

func TestBuildDeltaCandidates_Rejections(t *testing.T) {
	m := &memBlobs{}
	goodHash := putDelta(t, m, baseDelta("evt-1", []toolsnap.FileDelta{editFile("f.go", "x")}))

	// Serve valid bytes under the wrong hash.
	tampered := append([]byte(nil), m.blobs[goodHash]...)
	tampered = append(tampered, ' ')
	sum := sha256.Sum256(tampered)
	wrongHash := hex.EncodeToString(sum[:])
	m.blobs[wrongHash] = m.blobs[goodHash]
	garbageHash := m.put([]byte("not a delta"))

	unbound := baseDelta("evt-other", []toolsnap.FileDelta{editFile("f.go", "x")})
	unboundHash := putDelta(t, m, unbound)

	pair := baseDelta("evt-a", []toolsnap.FileDelta{editFile("f.go", "x")})
	pair.ToolUses = append(pair.ToolUses, toolsnap.ToolUse{
		ToolUseID: "toolu_b", ToolName: "Bash", EventID: "evt-b", Actor: 0,
	})
	pairHash := putDelta(t, m, pair)

	concurrent := baseDelta("evt-c", []toolsnap.FileDelta{editFile("f.go", "x")})
	concurrent.Scope = "concurrent_group"
	concurrent.Actors = append(concurrent.Actors, toolsnap.Actor{Provider: "codex", SessionID: "s2", TurnID: "t2"})
	concurrent.ToolUses = append(concurrent.ToolUses, toolsnap.ToolUse{
		ToolUseID: "toolu_c2", ToolName: "Bash", EventID: "evt-c2", Actor: 1,
	})
	concurrentHash := putDelta(t, m, concurrent)

	twoActors := baseDelta("evt-ta", []toolsnap.FileDelta{editFile("f.go", "x")})
	twoActors.Actors = append(twoActors.Actors, toolsnap.Actor{Provider: "codex", SessionID: "s2", TurnID: "t2"})
	twoActors.ToolUses = append(twoActors.ToolUses, toolsnap.ToolUse{
		ToolUseID: "toolu_ta2", ToolName: "Bash", EventID: "evt-ta2", Actor: 1,
	})
	twoActorsHash := putDelta(t, m, twoActors)
	codexLink := DeltaLink{EventID: "evt-ta2", EvidenceHash: twoActorsHash, GroupID: "g", Provider: "codex"}

	partial := baseDelta("evt-p", nil)
	partial.Status = "partial"
	partial.Reason = "post_snapshot_lost"
	partialHash := putDelta(t, m, partial)

	cases := []struct {
		name   string
		links  []DeltaLink
		reason string
	}{
		{"bad hash shape", []DeltaLink{link("e", "../escape", "g")}, DeltaRejectBadHash},
		{"uppercase hash", []DeltaLink{link("e", "AB"+goodHash[2:], "g")}, DeltaRejectBadHash},
		{"divergent hashes", []DeltaLink{
			link("evt-1", goodHash, "g"), link("evt-2", garbageHash, "g"),
		}, DeltaRejectDivergentHashes},
		{"blob unavailable", []DeltaLink{link("e", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "g")}, DeltaRejectBlobUnavailable},
		{"hash mismatch", []DeltaLink{link("evt-1", wrongHash, "g")}, DeltaRejectHashMismatch},
		{"parse failure", []DeltaLink{link("e", garbageHash, "g")}, DeltaRejectParseFailure},
		{"event binding", []DeltaLink{link("evt-not-in-delta", unboundHash, "g")}, DeltaRejectEventBinding},
		{"incomplete links", []DeltaLink{link("evt-a", pairHash, "g")}, DeltaRejectIncompleteLinks},
		{"provider binding", []DeltaLink{{EventID: "evt-1", EvidenceHash: goodHash, GroupID: "g", Provider: "codex"}}, DeltaRejectProviderBinding},
		{"concurrent group", []DeltaLink{link("evt-c", concurrentHash, "g"), codexLinkFor(concurrentHash, "evt-c2")}, DeltaRejectConcurrentGroup},
		{"ambiguous actors", []DeltaLink{link("evt-ta", twoActorsHash, "g"), codexLink}, DeltaRejectAmbiguousActors},
		{"partial", []DeltaLink{link("evt-p", partialHash, "g")}, DeltaRejectPartial},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := build(t, m, c.links)
			if out.Diags.GroupsEligible != 0 || out.Diags.Rejected[c.reason] != 1 {
				t.Fatalf("diags = %+v, want one %s rejection", out.Diags, c.reason)
			}
			if len(out.Claims)+len(out.Touched)+len(out.Deleted) != 0 {
				t.Fatalf("rejected group contributed evidence: %+v", out)
			}
		})
	}

	// Invalid hashes must not reach storage.
	m.gets = nil
	build(t, m, []DeltaLink{link("e", "../escape", "g")})
	if len(m.gets) != 0 {
		t.Fatalf("bad hash reached the blob store: %v", m.gets)
	}
}

func codexLinkFor(hash, eventID string) DeltaLink {
	return DeltaLink{EventID: eventID, EvidenceHash: hash, GroupID: "g", Provider: "codex"}
}

func TestBuildDeltaCandidates_FileClassification(t *testing.T) {
	m := &memBlobs{}
	files := []toolsnap.FileDelta{
		{Path: "bin.dat", Operation: "edit", BeforeHash: "a", AfterHash: "b",
			BeforeMode: "100644", AfterMode: "100644", Binary: true},
		{Path: "big.txt", Operation: "edit", BeforeHash: "a", AfterHash: "b",
			BeforeMode: "100644", AfterMode: "100644", Truncated: true},
		{Path: "link", Operation: "edit", BeforeHash: "a", AfterHash: "b",
			BeforeMode: "120000", AfterMode: "120000",
			Hunks: []toolsnap.Hunk{{OldStart: 1, NewStart: 1, NewCount: 1, NewLines: []string{"target"}}}},
		{Path: "sub", Operation: "edit", BeforeHash: "a", AfterHash: "b",
			BeforeMode: "160000", AfterMode: "160000"},
		{Path: "gone.go", Operation: "delete", BeforeHash: "a", AfterHash: "",
			BeforeMode: "100644", AfterMode: ""},
		{Path: "modeflip.sh", Operation: "typechange", BeforeHash: "a", AfterHash: "a",
			BeforeMode: "100644", AfterMode: "100755"},
		{Path: "typed.sh", Operation: "typechange", BeforeHash: "a", AfterHash: "b",
			BeforeMode: "100644", AfterMode: "100755",
			Hunks: []toolsnap.Hunk{{OldStart: 1, NewStart: 1, NewCount: 1, NewLines: []string{"#!/bin/sh"}}}},
		{Path: "waslink.go", Operation: "typechange", BeforeHash: "a", AfterHash: "b",
			BeforeMode: "120000", AfterMode: "100644",
			Hunks: []toolsnap.Hunk{{OldStart: 1, NewStart: 1, NewCount: 1, NewLines: []string{"package x"}}}},
		{Path: "madefile.go", Operation: "create", BeforeHash: "", AfterHash: "b",
			BeforeMode: "", AfterMode: "100644",
			Hunks: []toolsnap.Hunk{{OldStart: 1, NewStart: 1, NewCount: 1, NewLines: []string{"package y"}}}},
		{Path: "shrunk.go", Operation: "edit", BeforeHash: "a", AfterHash: "b",
			BeforeMode: "100644", AfterMode: "100644",
			Hunks: []toolsnap.Hunk{{OldStart: 1, OldCount: 2, NewStart: 1, NewCount: 0,
				OldLines: []string{"old1", "old2"}}}},
		{Path: "blanks.go", Operation: "edit", BeforeHash: "a", AfterHash: "b",
			BeforeMode: "100644", AfterMode: "100644",
			Hunks: []toolsnap.Hunk{{OldStart: 1, NewStart: 1, NewCount: 2, NewLines: []string{"", "  "}}}},
	}
	d := baseDelta("evt-1", files)
	d.Limits.Truncated = true
	hash := putDelta(t, m, d)
	out := build(t, m, []DeltaLink{link("evt-1", hash, "g1")})
	if out.Diags.GroupsEligible != 1 {
		t.Fatalf("diags = %+v", out.Diags)
	}
	for _, touch := range []string{"bin.dat", "big.txt", "link", "sub", "modeflip.sh", "waslink.go", "shrunk.go", "blanks.go"} {
		if !reflect.DeepEqual(out.Touched[touch], []string{"claude_code"}) {
			t.Fatalf("%s not touch-only: %+v", touch, out.Touched)
		}
	}
	if !reflect.DeepEqual(out.Deleted["gone.go"], []string{"claude_code"}) {
		t.Fatalf("deleted = %+v", out.Deleted)
	}
	if len(out.Claims["typed.sh"]) != 1 || out.Claims["typed.sh"][0].Line != "#!/bin/sh" {
		t.Fatalf("regular typechange lost line evidence: %+v", out.Claims)
	}
	if len(out.Claims["madefile.go"]) != 1 {
		t.Fatalf("create lost line evidence: %+v", out.Claims)
	}
	for _, noLines := range []string{"link", "waslink.go", "shrunk.go", "blanks.go"} {
		if len(out.Claims[noLines]) != 0 {
			t.Fatalf("%s produced line claims: %+v", noLines, out.Claims[noLines])
		}
	}
}

// Cancellation returns no partial candidates.
func TestBuildDeltaCandidates_CancellationPropagates(t *testing.T) {
	m := &memBlobs{}
	hash := putDelta(t, m, baseDelta("evt-1", []toolsnap.FileDelta{editFile("f.go", "x")}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := BuildDeltaCandidates(ctx, []DeltaLink{link("evt-1", hash, "g1")}, m)
	if !errors.Is(err, context.Canceled) || out != nil {
		t.Fatalf("out=%v err=%v, want context.Canceled and nil result", out, err)
	}

	// Propagate cancellation returned by storage.
	failing := &cancelBlobs{}
	out, err = BuildDeltaCandidates(context.Background(), []DeltaLink{link("evt-1", hash, "g1")}, failing)
	if !errors.Is(err, context.DeadlineExceeded) || out != nil {
		t.Fatalf("out=%v err=%v, want context.DeadlineExceeded and nil result", out, err)
	}

	// Detect cancellation even when storage returns valid bytes.
	ctx2, cancel2 := context.WithCancel(context.Background())
	ignoring := &cancelDuringGet{blobs: m, cancel: cancel2}
	out, err = BuildDeltaCandidates(ctx2, []DeltaLink{link("evt-1", hash, "g1")}, ignoring)
	if !errors.Is(err, context.Canceled) || out != nil {
		t.Fatalf("out=%v err=%v, want context.Canceled despite successful read", out, err)
	}

	// Detect cancellation after hashing and parsing.
	counting := &countingErrCtx{Context: context.Background(), failAt: 3}
	out, err = BuildDeltaCandidates(counting, []DeltaLink{link("evt-1", hash, "g1")}, m)
	if !errors.Is(err, context.Canceled) || out != nil {
		t.Fatalf("out=%v err=%v, want context.Canceled from the post-parse check", out, err)
	}
	if counting.calls < 3 {
		t.Fatalf("Err() called %d times; the post-parse check never ran", counting.calls)
	}
}

// countingErrCtx cancels on a chosen Err call.
type countingErrCtx struct {
	context.Context
	calls  int
	failAt int
}

func (c *countingErrCtx) Err() error {
	c.calls++
	if c.calls >= c.failAt {
		return context.Canceled
	}
	return nil
}

type cancelBlobs struct{}

func (cancelBlobs) Get(context.Context, string) ([]byte, error) {
	return nil, context.DeadlineExceeded
}

// cancelDuringGet cancels while returning valid bytes.
type cancelDuringGet struct {
	blobs  *memBlobs
	cancel context.CancelFunc
}

func (c *cancelDuringGet) Get(ctx context.Context, hash string) ([]byte, error) {
	c.cancel()
	return c.blobs.Get(ctx, hash)
}

// Touch and deletion evidence retains every provider.
func TestBuildDeltaCandidates_MultiProviderInvolvement(t *testing.T) {
	m := &memBlobs{}
	binFile := toolsnap.FileDelta{Path: "shared.dat", Operation: "edit",
		BeforeHash: "a", AfterHash: "b", BeforeMode: "100644", AfterMode: "100644", Binary: true}
	delFile := toolsnap.FileDelta{Path: "shared-del.go", Operation: "delete",
		BeforeHash: "a", AfterHash: "", BeforeMode: "100644", AfterMode: ""}

	claude := baseDelta("evt-cl", []toolsnap.FileDelta{binFile, delFile})
	claudeHash := putDelta(t, m, claude)

	codex := baseDelta("evt-cx", []toolsnap.FileDelta{binFile, delFile})
	codex.Actors[0].Provider = "codex"
	codex.ToolUses[0].ToolUseID = "toolu_cx"
	codexHash := putDelta(t, m, codex)

	out := build(t, m, []DeltaLink{
		link("evt-cl", claudeHash, "g1"),
		{EventID: "evt-cx", EvidenceHash: codexHash, GroupID: "g2", Provider: "codex"},
	})
	if out.Diags.GroupsEligible != 2 {
		t.Fatalf("diags = %+v", out.Diags)
	}
	if !reflect.DeepEqual(out.Touched["shared.dat"], []string{"claude_code", "codex"}) {
		t.Fatalf("touched = %+v, want both providers in first-seen order", out.Touched)
	}
	if !reflect.DeepEqual(out.Deleted["shared-del.go"], []string{"claude_code", "codex"}) {
		t.Fatalf("deleted = %+v, want both providers in first-seen order", out.Deleted)
	}
}

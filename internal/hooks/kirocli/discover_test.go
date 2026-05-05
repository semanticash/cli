package kirocli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/hooks"
)

// kiroDiscoverFixture writes only the header fields used by discovery.
type kiroDiscoverFixture struct {
	cwd       string
	createdAt time.Time // header created_at; <= PromptTime marks parent-shaped
}

// writeKiroSessionPair writes a .json header and empty .jsonl pair with the
// given mtime, mirroring the on-disk layout of a Kiro CLI session.
func writeKiroSessionPair(t *testing.T, dir, sessionID string, fx kiroDiscoverFixture, mtime time.Time) string {
	t.Helper()
	header := map[string]any{
		"session_id": sessionID,
		"cwd":        fx.cwd,
		"created_at": fx.createdAt.UTC().Format(time.RFC3339Nano),
	}
	data, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	jsonPath := filepath.Join(dir, sessionID+".json")
	jsonlPath := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(jsonPath, data, 0o644); err != nil {
		t.Fatalf("write json: %v", err)
	}
	if err := os.WriteFile(jsonlPath, []byte{}, 0o644); err != nil {
		t.Fatalf("write jsonl: %v", err)
	}
	if err := os.Chtimes(jsonlPath, mtime, mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	return jsonlPath
}

func TestDiscoverSubagentTranscripts_ZeroChildren(t *testing.T) {
	dir := t.TempDir()
	prompt := time.Now()
	mtime := prompt.Add(10 * time.Second)
	// Only a parent-shaped session in the same repo.
	writeKiroSessionPair(t, dir, "parent-1",
		kiroDiscoverFixture{cwd: "/repo", createdAt: prompt.Add(-time.Second)},
		mtime,
	)

	p := &Provider{sessionsDir: dir}
	dctx := hooks.DiscoveryContext{
		Cwd:        "/repo",
		PromptTime: prompt.UnixMilli(),
		StopTime:   prompt.Add(time.Minute).UnixMilli(),
	}
	paths, err := p.DiscoverSubagentTranscripts(context.Background(), "", dctx)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths = %v, want []", paths)
	}
}

func TestDiscoverSubagentTranscripts_OneChild(t *testing.T) {
	dir := t.TempDir()
	prompt := time.Now()
	mtime := prompt.Add(10 * time.Second)
	// Parents have agent_name; AgentCrew children do not.
	writeKiroSessionPair(t, dir, "parent-1",
		kiroDiscoverFixture{cwd: "/repo", createdAt: prompt.Add(-time.Second)},
		mtime,
	)
	childPath := writeKiroSessionPair(t, dir, "child-1",
		kiroDiscoverFixture{cwd: "/repo", createdAt: prompt.Add(time.Second)},
		mtime,
	)

	p := &Provider{sessionsDir: dir}
	dctx := hooks.DiscoveryContext{
		Cwd:        "/repo",
		PromptTime: prompt.UnixMilli(),
		StopTime:   prompt.Add(time.Minute).UnixMilli(),
	}
	paths, err := p.DiscoverSubagentTranscripts(context.Background(), "", dctx)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(paths) != 1 || paths[0] != childPath {
		t.Errorf("paths = %v, want [%s]", paths, childPath)
	}
}

func TestDiscoverSubagentTranscripts_NegativeDiscovery(t *testing.T) {
	// A concurrent user session in the same repo makes the window ambiguous.
	dir := t.TempDir()
	prompt := time.Now()
	mtime := prompt.Add(10 * time.Second)
	writeKiroSessionPair(t, dir, "parent-1",
		kiroDiscoverFixture{cwd: "/repo", createdAt: prompt.Add(-time.Second)},
		mtime,
	)
	writeKiroSessionPair(t, dir, "concurrent-user",
		kiroDiscoverFixture{cwd: "/repo", createdAt: prompt.Add(-time.Second)},
		mtime,
	)

	p := &Provider{sessionsDir: dir}
	dctx := hooks.DiscoveryContext{
		Cwd:        "/repo",
		PromptTime: prompt.UnixMilli(),
		StopTime:   prompt.Add(time.Minute).UnixMilli(),
	}
	paths, err := p.DiscoverSubagentTranscripts(context.Background(), "", dctx)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths = %v, want [] (concurrent user session must not match)", paths)
	}
}

func TestDiscoverSubagentTranscripts_NoParentMatchFailClosed(t *testing.T) {
	// A child-shaped session lands in the same cwd/window but no
	// parent-shaped session matches. This happens when the parent's
	// header is not yet flushed, when its mtime falls outside the
	// window, or when its schema is one we do not parse. Without a
	// positive parent anchor, discovery drops the candidate.
	dir := t.TempDir()
	prompt := time.Now()
	mtime := prompt.Add(10 * time.Second)
	writeKiroSessionPair(t, dir, "child-orphan",
		kiroDiscoverFixture{cwd: "/repo", createdAt: prompt.Add(time.Second)},
		mtime,
	)

	p := &Provider{sessionsDir: dir}
	dctx := hooks.DiscoveryContext{
		Cwd:        "/repo",
		PromptTime: prompt.UnixMilli(),
		StopTime:   prompt.Add(time.Minute).UnixMilli(),
	}
	paths, err := p.DiscoverSubagentTranscripts(context.Background(), "", dctx)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths = %v, want [] (no parent-shaped session means no anchor)", paths)
	}
}

func TestDiscoverSubagentTranscripts_ConcurrentParentsFailClosed(t *testing.T) {
	// Two user-driven parent sessions in the same repo overlap with
	// one child-shaped session. Child files carry no pointer back to
	// either parent, so discovery returns no children.
	dir := t.TempDir()
	prompt := time.Now()
	mtime := prompt.Add(10 * time.Second)
	writeKiroSessionPair(t, dir, "parent-A",
		kiroDiscoverFixture{cwd: "/repo", createdAt: prompt.Add(-time.Second)},
		mtime,
	)
	writeKiroSessionPair(t, dir, "parent-B",
		kiroDiscoverFixture{cwd: "/repo", createdAt: prompt.Add(-time.Second)},
		mtime,
	)
	// This child would match if only one parent were present.
	writeKiroSessionPair(t, dir, "child-1",
		kiroDiscoverFixture{cwd: "/repo", createdAt: prompt.Add(time.Second)},
		mtime,
	)

	p := &Provider{sessionsDir: dir}
	dctx := hooks.DiscoveryContext{
		Cwd:        "/repo",
		PromptTime: prompt.UnixMilli(),
		StopTime:   prompt.Add(time.Minute).UnixMilli(),
	}
	paths, err := p.DiscoverSubagentTranscripts(context.Background(), "", dctx)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths = %v, want [] (concurrent parents must fail closed)", paths)
	}
}

func TestDiscoverSubagentTranscripts_DifferentRepoExcluded(t *testing.T) {
	// A child-shaped session in a different repo is concurrent
	// activity from another Kiro CLI invocation. The cwd guard keeps
	// it out without depending on timing.
	dir := t.TempDir()
	prompt := time.Now()
	mtime := prompt.Add(10 * time.Second)
	writeKiroSessionPair(t, dir, "child-other-repo",
		kiroDiscoverFixture{cwd: "/other", createdAt: prompt.Add(time.Second)},
		mtime,
	)

	p := &Provider{sessionsDir: dir}
	dctx := hooks.DiscoveryContext{
		Cwd:        "/repo",
		PromptTime: prompt.UnixMilli(),
		StopTime:   prompt.Add(time.Minute).UnixMilli(),
	}
	paths, err := p.DiscoverSubagentTranscripts(context.Background(), "", dctx)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths = %v, want [] (different cwd must be excluded)", paths)
	}
}

func TestDiscoverSubagentTranscripts_OutOfWindowExcluded(t *testing.T) {
	// A child-shaped file from a previous turn must not attach to
	// this turn even if cwd matches. Mtime older than PromptTime is
	// the boundary condition.
	dir := t.TempDir()
	prompt := time.Now()
	stale := prompt.Add(-time.Hour)
	writeKiroSessionPair(t, dir, "child-stale",
		kiroDiscoverFixture{cwd: "/repo", createdAt: prompt.Add(time.Second)},
		stale,
	)

	p := &Provider{sessionsDir: dir}
	dctx := hooks.DiscoveryContext{
		Cwd:        "/repo",
		PromptTime: prompt.UnixMilli(),
		StopTime:   prompt.Add(time.Minute).UnixMilli(),
	}
	paths, err := p.DiscoverSubagentTranscripts(context.Background(), "", dctx)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(paths) != 0 {
		t.Errorf("paths = %v, want [] (mtime older than PromptTime)", paths)
	}
}

func TestDiscoverSubagentTranscripts_MissingContextReturnsNothing(t *testing.T) {
	// Missing cwd or PromptTime means we cannot tell child from
	// concurrent unrelated sessions.
	dir := t.TempDir()
	now := time.Now()
	writeKiroSessionPair(t, dir, "child-1",
		kiroDiscoverFixture{cwd: "/repo", createdAt: time.Now().Add(time.Second)},
		now,
	)

	p := &Provider{sessionsDir: dir}

	for _, tc := range []struct {
		name string
		dctx hooks.DiscoveryContext
	}{
		{"missing cwd", hooks.DiscoveryContext{PromptTime: time.Now().UnixMilli()}},
		{"missing prompt time", hooks.DiscoveryContext{Cwd: "/repo"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			paths, err := p.DiscoverSubagentTranscripts(context.Background(), "", tc.dctx)
			if err != nil {
				t.Fatalf("discover: %v", err)
			}
			if len(paths) != 0 {
				t.Errorf("paths = %v, want [] when context is incomplete", paths)
			}
		})
	}
}

func TestDiscoverSubagentTranscripts_MissingDirIsEmpty(t *testing.T) {
	p := &Provider{sessionsDir: filepath.Join(t.TempDir(), "does-not-exist")}
	dctx := hooks.DiscoveryContext{
		Cwd:        "/repo",
		PromptTime: time.Now().UnixMilli(),
		StopTime:   time.Now().Add(time.Minute).UnixMilli(),
	}
	paths, err := p.DiscoverSubagentTranscripts(context.Background(), "", dctx)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if paths != nil {
		t.Errorf("paths = %v, want nil for missing sessions dir", paths)
	}
}

func TestSubagentStateKey_KiroCLI(t *testing.T) {
	// The state key must round-trip the Kiro session UUID and be safe
	// for use as a filename component (no path separators).
	p := &Provider{}
	got := p.SubagentStateKey("/abs/path/to/abc-123-def.jsonl")
	want := "kirocli-subagent-abc-123-def"
	if got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
}

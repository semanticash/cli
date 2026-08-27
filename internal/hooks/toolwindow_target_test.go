package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestToolWindowCWD_Precedence(t *testing.T) {
	cases := []struct {
		name  string
		event *Event
		state *CaptureState
		want  string
	}{
		{"effective wins", &Event{EffectiveCWD: "/cmd", CWD: "/session"}, &CaptureState{CWD: "/state"}, "/cmd"},
		{"session when no effective", &Event{CWD: "/session"}, &CaptureState{CWD: "/state"}, "/session"},
		{"state when event empty", &Event{}, &CaptureState{CWD: "/state"}, "/state"},
		{"empty when all absent", &Event{}, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := toolWindowCWD(c.event, c.state); got != c.want {
				t.Errorf("toolWindowCWD = %q, want %q", got, c.want)
			}
		})
	}
}

func key(provider, session, turn, tool string) toolWindowReceiptKey {
	return toolWindowReceiptKey{Provider: provider, SessionID: session, TurnID: turn, ToolUseID: tool}
}

// The sweep removes expired targets and keeps fresh ones.
func TestSweepToolWindowTargets(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	orig := toolWindowNow
	t.Cleanup(func() { toolWindowNow = orig })
	base := int64(10_000_000)
	toolWindowNow = func() int64 { return base }

	fresh := key("cursor", "s-fresh", "t", "call")
	stale := key("cursor", "s-stale", "t", "call")
	if err := SaveToolWindowTarget(fresh, "/repo", "id"); err != nil {
		t.Fatal(err)
	}
	if err := SaveToolWindowTarget(stale, "/repo", "id"); err != nil {
		t.Fatal(err)
	}

	// Advance past the TTL and refresh one target.
	toolWindowNow = func() int64 { return base + toolWindowTargetTTL.Milliseconds() + 1000 }
	if err := SaveToolWindowTarget(fresh, "/repo", "id"); err != nil {
		t.Fatal(err)
	}

	n, err := SweepToolWindowTargets()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Errorf("removed = %d, want 1 (stale only)", n)
	}
	if rec, err := LoadToolWindowTarget(fresh); err != nil || rec == nil {
		t.Errorf("fresh receipt removed: (%+v, %v)", rec, err)
	}
	if rec, err := LoadToolWindowTarget(stale); err != nil || rec != nil {
		t.Errorf("stale receipt not swept: (%+v, %v)", rec, err)
	}
}

// The sweep skips non-regular entries and reports read failures.
func TestSweepToolWindowTargets_SkipsNonRegularAndSurfacesErrors(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	dir, err := captureDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Matching directories are not followed or removed.
	subDir := filepath.Join(dir, "toolwindow-dir.json")
	if err := os.Mkdir(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	n, err := SweepToolWindowTargets()
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Errorf("removed = %d, want 0 (non-regular entry skipped)", n)
	}
	if _, statErr := os.Stat(subDir); statErr != nil {
		t.Errorf("receipt-named directory was followed/removed: %v", statErr)
	}

	// Root ignores file permissions, so it cannot exercise this case.
	if os.Geteuid() > 0 {
		bad := filepath.Join(dir, "toolwindow-bad.json")
		if err := os.WriteFile(bad, []byte("{}"), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(bad, 0o644) })
		if _, err := SweepToolWindowTargets(); err == nil {
			t.Error("expected an error for an unreadable receipt")
		}
	}
}

// A target survives validation and can be deleted.
func TestToolWindowTarget_Roundtrip(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	k := key("cursor", "sess-1", "turn-1", "call 7")

	if err := SaveToolWindowTarget(k, "/work/repoB", "repo-b-id"); err != nil {
		t.Fatalf("save: %v", err)
	}
	rec, err := LoadToolWindowTarget(k)
	if err != nil || rec == nil {
		t.Fatalf("load: rec=%+v err=%v", rec, err)
	}
	if rec.RepoPath != "/work/repoB" || rec.RepositoryID != "repo-b-id" || rec.Version != toolWindowTargetVersion {
		t.Errorf("record = %+v", rec)
	}
	if err := DeleteToolWindowTarget(k); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if rec, err := LoadToolWindowTarget(k); err != nil || rec != nil {
		t.Errorf("after delete: rec=%+v err=%v, want (nil,nil)", rec, err)
	}
	if err := DeleteToolWindowTarget(k); err != nil {
		t.Errorf("delete absent: %v", err)
	}
}

// Similar window identities produce distinct target files.
func TestToolWindowTarget_CollisionFree(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	k1 := key("cursor", "batch/a", "turn-1", "call 7")
	k2 := key("cursor", "retry/a", "turn-1", "call 7")

	if err := SaveToolWindowTarget(k1, "/repo1", "id1"); err != nil {
		t.Fatal(err)
	}
	if err := SaveToolWindowTarget(k2, "/repo2", "id2"); err != nil {
		t.Fatal(err)
	}
	r1, _ := LoadToolWindowTarget(k1)
	r2, _ := LoadToolWindowTarget(k2)
	if r1 == nil || r2 == nil || r1.RepoPath != "/repo1" || r2.RepoPath != "/repo2" {
		t.Errorf("collision: r1=%+v r2=%+v, want distinct repos", r1, r2)
	}
}

// Expired targets are rejected.
func TestToolWindowTarget_Expired(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	orig := toolWindowNow
	t.Cleanup(func() { toolWindowNow = orig })
	base := int64(1_000_000)
	toolWindowNow = func() int64 { return base }

	k := key("cursor", "sess-e", "turn-1", "call 7")
	if err := SaveToolWindowTarget(k, "/repo", "id"); err != nil {
		t.Fatal(err)
	}
	toolWindowNow = func() int64 { return base + toolWindowTargetTTL.Milliseconds() + 1 }
	if rec, err := LoadToolWindowTarget(k); err == nil || rec != nil {
		t.Errorf("expired load = (%+v, %v), want fail closed", rec, err)
	}
}

// A target with a different identity is rejected.
func TestToolWindowTarget_IdentityMismatch(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	k := key("cursor", "sess-m", "turn-1", "call 7")
	path, err := toolWindowTargetPath(k)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	bad := ToolWindowTargetRecord{
		Version: toolWindowTargetVersion, CreatedAt: toolWindowNow(),
		Provider: "codex", SessionID: "sess-m", TurnID: "turn-1", ToolUseID: "call 7",
		RepoPath: "/repo", RepositoryID: "id",
	}
	data, _ := json.Marshal(bad)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if rec, err := LoadToolWindowTarget(k); err == nil || rec != nil {
		t.Errorf("identity mismatch load = (%+v, %v), want fail closed", rec, err)
	}
}

// Save rejects an incomplete identity or an empty repository.
func TestToolWindowTarget_SaveRejectsInvalid(t *testing.T) {
	t.Setenv("SEMANTICA_HOME", t.TempDir())
	if err := SaveToolWindowTarget(key("cursor", "", "t", "call"), "/repo", "id"); err == nil {
		t.Error("expected error for empty session id")
	}
	if err := SaveToolWindowTarget(key("cursor", "s", "t", "call"), "", "id"); err == nil {
		t.Error("expected error for empty repo path")
	}
	if err := SaveToolWindowTarget(key("cursor", "s", "t", "call"), "/repo", ""); err == nil {
		t.Error("expected error for empty repository id")
	}
}

package util

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/platform"
)

// shrinkRoutingLockBound reduces retry delays and restores settings on cleanup.
func shrinkRoutingLockBound(t *testing.T) {
	t.Helper()
	origAttempts, origDelay := routingLockAttempts, routingLockRetryDelay
	routingLockAttempts, routingLockRetryDelay = 3, time.Millisecond
	t.Cleanup(func() { routingLockAttempts, routingLockRetryDelay = origAttempts, origDelay })
}

// seedRoutingConfigDir isolates the log through XDG_CONFIG_HOME.
func seedRoutingConfigDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	return filepath.Join(root, "semantica")
}

func TestRoutingDecisions_RoundTrip(t *testing.T) {
	seedRoutingConfigDir(t)

	AppendRoutingDecisions([]RoutingDecisionEntry{
		{EventID: "e1", Provider: "codex", Signal: "structured_path", Repo: "/repos/api", SessionRepo: "/repos/cli", SignalDiff: true, Persisted: true},
		{EventID: "e2", Provider: "codex", Signal: "launch_fallback", Repo: "/repos/cli", Persisted: false},
		{EventID: "e3", Provider: "codex", Signal: "unresolved"},
	})

	got, err := ReadRoutingDecisionTail(100)
	if err != nil {
		t.Fatalf("read tail: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("entries = %d, want 3", len(got))
	}
	if got[0].EventID != "e1" || got[0].Signal != "structured_path" || got[0].Repo != "/repos/api" ||
		!got[0].SignalDiff || !got[0].Persisted || got[0].Timestamp == "" {
		t.Errorf("entry 0 = %+v", got[0])
	}
	if got[2].Signal != "unresolved" || got[2].Repo != "" || got[2].Persisted {
		t.Errorf("unresolved entry = %+v", got[2])
	}
}

// Each destination retains its own persistence outcome.
func TestRoutingDecisions_PerDestinationFanOut(t *testing.T) {
	seedRoutingConfigDir(t)
	AppendRoutingDecisions([]RoutingDecisionEntry{
		{EventID: "e1", Signal: "structured_path", Repo: "/repos/api", Persisted: true},
		{EventID: "e1", Signal: "structured_path", Repo: "/repos/cli", Persisted: false},
	})
	got, err := ReadRoutingDecisionTail(100)
	if err != nil {
		t.Fatalf("read tail: %v", err)
	}
	byRepo := map[string]bool{}
	for _, e := range got {
		if e.EventID == "e1" {
			byRepo[e.Repo] = e.Persisted
		}
	}
	if byRepo["/repos/api"] != true || byRepo["/repos/cli"] != false {
		t.Fatalf("per-destination persistence not represented: %+v", byRepo)
	}
}

// Concurrent appends preserve every record as valid JSON.
func TestRoutingDecisions_ConcurrentAppendsAreNotTorn(t *testing.T) {
	dir := seedRoutingConfigDir(t)
	const workers, per = 8, 25

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				AppendRoutingDecisions([]RoutingDecisionEntry{
					{EventID: fmt.Sprintf("w%d-e%d", w, i), Signal: "launch_fallback", Repo: "/r", Persisted: true},
				})
			}
		}(w)
	}
	wg.Wait()

	body, err := os.ReadFile(filepath.Join(dir, RoutingDecisionLogBasename))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != workers*per {
		t.Fatalf("lines = %d, want %d", len(lines), workers*per)
	}
	for _, ln := range lines {
		var e RoutingDecisionEntry
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("torn/invalid line %q: %v", ln, err)
		}
	}
}

// Rotation retains complete records within the size limit plus batch overhead.
func TestRoutingDecisions_RotationCutsOnRecordBoundary(t *testing.T) {
	dir := seedRoutingConfigDir(t)

	orig := routingDecisionLogMaxSize
	routingDecisionLogMaxSize = 2048
	defer func() { routingDecisionLogMaxSize = orig }()

	for i := 0; i < 500; i++ {
		AppendRoutingDecisions([]RoutingDecisionEntry{
			{EventID: fmt.Sprintf("e%d", i), Signal: "launch_fallback", Repo: "/repos/cli", Persisted: true},
		})
	}

	path := filepath.Join(dir, RoutingDecisionLogBasename)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Bounded: cap plus at most one batch beyond the trigger.
	if info.Size() > routingDecisionLogMaxSize*2 {
		t.Fatalf("size = %d, want <= %d", info.Size(), routingDecisionLogMaxSize*2)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, ln := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if ln == "" {
			continue
		}
		var e RoutingDecisionEntry
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("rotation split a record: %q: %v", ln, err)
		}
	}
}

// Readers skip an incomplete trailing record.
func TestRoutingDecisions_TornTrailingLineSkipped(t *testing.T) {
	dir := seedRoutingConfigDir(t)
	AppendRoutingDecisions([]RoutingDecisionEntry{{EventID: "e1", Signal: "unresolved"}})

	path := filepath.Join(dir, RoutingDecisionLogBasename)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, _ = f.WriteString(`{"event_id":"e2","signal":"lau`) // truncated, no newline
	_ = f.Close()

	got, err := ReadRoutingDecisionTail(100)
	if err != nil {
		t.Fatalf("read tail: %v", err)
	}
	if len(got) != 1 || got[0].EventID != "e1" {
		t.Fatalf("entries = %+v, want only the intact e1", got)
	}
}

// A held file lock causes appends to skip after bounded retries.
func TestRoutingDecisions_SkipsWhenSidecarLockHeld(t *testing.T) {
	dir := seedRoutingConfigDir(t)
	shrinkRoutingLockBound(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lockPath := filepath.Join(dir, RoutingDecisionLogBasename+".lock")
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	locked, err := platform.TryLockFile(lf)
	if err != nil || !locked {
		t.Fatalf("hold sidecar lock: locked=%v err=%v", locked, err)
	}
	defer func() { _ = platform.UnlockFile(lf); _ = lf.Close() }()

	done := make(chan struct{})
	go func() {
		AppendRoutingDecisions([]RoutingDecisionEntry{{EventID: "e1", Signal: "unresolved"}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("append did not return within bound while sidecar lock held")
	}

	got, _ := ReadRoutingDecisionTail(100)
	if len(got) != 0 {
		t.Fatalf("expected skip while sidecar lock held, got %d entries", len(got))
	}
}

// A held mutex causes appends to skip after bounded retries.
func TestRoutingDecisions_SkipsWhenMutexContended(t *testing.T) {
	seedRoutingConfigDir(t)
	shrinkRoutingLockBound(t)

	routingDecisionMu.Lock()
	done := make(chan struct{})
	go func() {
		AppendRoutingDecisions([]RoutingDecisionEntry{{EventID: "e1", Signal: "unresolved"}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		routingDecisionMu.Unlock()
		t.Fatal("append did not return within bound while mutex held")
	}
	routingDecisionMu.Unlock()

	got, _ := ReadRoutingDecisionTail(100)
	if len(got) != 0 {
		t.Fatalf("expected skip while mutex held, got %d entries", len(got))
	}
}

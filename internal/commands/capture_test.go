package commands

import (
	"io"
	"strings"
	"testing"
	"time"
)

// readHookPayload is the bounded stdin reader used by `semantica
// capture`. These tests cover the behaviors capture relies on:
//
//  1. Piped readers return their bytes before the deadline.
//  2. Closed empty readers return an empty payload without timing out.
//  3. Open readers time out instead of blocking indefinitely.

func TestReadHookPayload_PipedReaderReturnsBytes(t *testing.T) {
	const want = `{"session_id":"sess-1","tool_name":"Edit"}`
	payload, timedOut, err := readHookPayload(strings.NewReader(want), 100*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timedOut {
		t.Errorf("timedOut=true on a closed reader; want false")
	}
	if string(payload) != want {
		t.Errorf("payload = %q, want %q", payload, want)
	}
}

func TestReadHookPayload_EmptyClosedReaderReturnsEmpty(t *testing.T) {
	// io.Pipe with an immediately-closed writer mimics a hook runner
	// that opens stdin but writes nothing before closing it.
	pr, pw := io.Pipe()
	_ = pw.Close()
	payload, timedOut, err := readHookPayload(pr, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if timedOut {
		t.Errorf("timedOut=true on a closed empty reader; want false")
	}
	if len(payload) != 0 {
		t.Errorf("payload = %q, want empty", payload)
	}
}

func TestReadHookPayload_OpenReaderTimesOut(t *testing.T) {
	// pw is intentionally left open to mimic hosts that inherit stdin
	// instead of piping and closing a hook payload.
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	const deadline = 50 * time.Millisecond
	start := time.Now()
	payload, timedOut, err := readHookPayload(pr, deadline)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !timedOut {
		t.Errorf("timedOut=false on an open reader; want true")
	}
	if payload != nil {
		t.Errorf("payload = %q, want nil on timeout", payload)
	}
	// Allow generous slack for slow CI, but fail if the read is
	// effectively unbounded.
	maxElapsed := 10 * deadline
	if elapsed > maxElapsed {
		t.Errorf("readHookPayload took %v on a never-closing reader; want under %v", elapsed, maxElapsed)
	}
}

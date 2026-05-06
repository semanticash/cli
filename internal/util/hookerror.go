package util

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/semanticash/cli/internal/platform"
)

// HookErrorLogBasename is the filename used for the global capture
// sidecar log. Hooks themselves remain non-blocking (`|| true` is
// preserved); this file gives doctor a place to read
// failures that would otherwise be silent.
const HookErrorLogBasename = "hook-errors.log"

// hookErrorLogMaxSize is the soft cap before the file is truncated.
// At cap, the file is rewritten with the most recent half of its
// content.
const hookErrorLogMaxSize = 5 * 1024 * 1024

// HookErrorEntry is the JSONL shape doctor consumes when grouping
// hook errors by provider/hook.
type HookErrorEntry struct {
	Timestamp string `json:"ts"`
	Provider  string `json:"provider,omitempty"`
	Hook      string `json:"hook,omitempty"`
	Message   string `json:"msg"`
}

// hookErrorMu serializes appends within a single process. Doctor and
// the capture command may run concurrently; the underlying file is
// opened with O_APPEND so cross-process atomicity is delegated to the
// kernel for line-sized writes.
var hookErrorMu sync.Mutex

// AppendHookError writes a JSONL entry to the global sidecar log at
// <AppConfigDir>/hook-errors.log. Write failures are ignored so
// diagnostics never block provider hooks.
//
// The log directory is auto-created. Provider and hook may be empty
// for failures that do not have that context (e.g. unknown provider).
func AppendHookError(provider, hook, msg string) {
	dir, err := AppConfigDir()
	if err != nil {
		return
	}

	hookErrorMu.Lock()
	defer hookErrorMu.Unlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, HookErrorLogBasename)

	if err := truncateIfTooLarge(path); err != nil {
		// Truncation is best-effort; continue to append even on failure.
		_ = err
	}

	entry := HookErrorEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Provider:  provider,
		Hook:      hook,
		Message:   msg,
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	line = append(line, '\n')

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	_, _ = f.Write(line)
}

// truncateIfTooLarge keeps the file under hookErrorLogMaxSize by
// rewriting it with the trailing half of its content when oversize.
// The cut point is aligned to a newline boundary so we never split
// a JSON record.
func truncateIfTooLarge(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() < hookErrorLogMaxSize {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	// Read the trailing half, then advance to the next newline so
	// the surviving content starts on a record boundary.
	keep := info.Size() / 2
	if _, err := f.Seek(info.Size()-keep, io.SeekStart); err != nil {
		return err
	}
	tail, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if i := indexOfByte(tail, '\n'); i >= 0 {
		tail = tail[i+1:]
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, tail, 0o644); err != nil {
		return err
	}
	// SafeRename replaces the existing log on platforms where
	// os.Rename does not overwrite destinations.
	return platform.SafeRename(tmp, path)
}

func indexOfByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}

// ReadHookErrorTail reads up to maxLines most-recent JSONL entries
// from the hook-errors.log. Malformed or truncated lines are
// silently skipped so a partially-written record near the end of
// the file does not break diagnostics.
func ReadHookErrorTail(maxLines int) ([]HookErrorEntry, error) {
	dir, err := AppConfigDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, HookErrorLogBasename)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	body, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	lines := splitLines(body)

	start := 0
	if len(lines) > maxLines {
		start = len(lines) - maxLines
	}

	entries := make([]HookErrorEntry, 0, len(lines)-start)
	for _, line := range lines[start:] {
		if len(line) == 0 {
			continue
		}
		var e HookErrorEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// HookErrorLogPath exposes the sidecar log location for diagnostic
// messages.
func HookErrorLogPath() (string, error) {
	dir, err := AppConfigDir()
	if err != nil {
		return "", fmt.Errorf("hook-errors log path: %w", err)
	}
	return filepath.Join(dir, HookErrorLogBasename), nil
}

func splitLines(b []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, v := range b {
		if v == '\n' {
			lines = append(lines, b[start:i])
			start = i + 1
		}
	}
	if start < len(b) {
		lines = append(lines, b[start:])
	}
	return lines
}

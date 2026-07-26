//go:build windows

package platform

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func init() {
	helperModes["renameholder"] = helperRenameHolder
}

// helperRenameHolder blocks replacement until its release signal appears.
func helperRenameHolder() {
	path := os.Getenv("SEMANTICA_HELPER_HOLD_PATH")
	releasePath := os.Getenv("SEMANTICA_HELPER_RELEASE_PATH")
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode path: %v\n", err)
		os.Exit(2)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open with no sharing: %v\n", err)
		os.Exit(2)
	}
	fmt.Println("HOLDING")
	// Bound the helper lifetime if its parent exits unexpectedly.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(releasePath); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = windows.CloseHandle(h)
	fmt.Println("RELEASED")
}

// TestFileReplace_IntegrationRetriesOnTransient drives each
// replacement primitive through a real sharing violation and verifies
// its bounded retry recovers once the holder releases.
func TestFileReplace_IntegrationRetriesOnTransient(t *testing.T) {
	if testing.Short() {
		t.Skip("integration: retry under contention test")
	}
	for _, tc := range []struct {
		name string
		fn   func(src, dst string) error
	}{
		{"SafeRename", SafeRename},
		{"ReplaceFile", ReplaceFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runTransientContention(t, tc.name, tc.fn)
		})
	}
}

func runTransientContention(t *testing.T, name string, replace func(src, dst string) error) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	releasePath := filepath.Join(dir, "release-signal")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := startHelper(t, "renameholder",
		"SEMANTICA_HELPER_HOLD_PATH="+dst,
		"SEMANTICA_HELPER_RELEASE_PATH="+releasePath,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(releasePath, nil, 0o644)
		_ = cmd.Wait()
	})

	scanner := bufio.NewScanner(stdout)
	holding := false
	for scanner.Scan() {
		if scanner.Text() == "HOLDING" {
			holding = true
			break
		}
	}
	if !holding {
		t.Fatal("holder never signaled HOLDING")
	}

	released := make(chan struct{})
	go func() {
		for scanner.Scan() {
			if scanner.Text() == "RELEASED" {
				close(released)
				return
			}
		}
	}()

	retries := 0
	orig := renameRetrySleep
	renameRetrySleep = func(time.Duration) {
		retries++
		if retries > 1 {
			return
		}
		// Release contention before the next retry.
		if err := os.WriteFile(releasePath, nil, 0o644); err != nil {
			t.Fatalf("write release signal: %v", err)
		}
		select {
		case <-released:
		case <-time.After(10 * time.Second):
			t.Fatal("holder never confirmed release")
		}
	}
	t.Cleanup(func() { renameRetrySleep = orig })

	if err := replace(src, dst); err != nil {
		t.Fatalf("%s did not recover from transient sharing violation: %v", name, err)
	}
	if retries == 0 {
		t.Fatalf("%s succeeded without hitting the transient path; retry not exercised", name)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("dst content = %q, want new", got)
	}
}

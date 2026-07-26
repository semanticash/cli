package platform

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHelperProcess runs subprocess modes used by integration tests.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("SEMANTICA_PLATFORM_HELPER")
	if mode == "" {
		return
	}
	if fn, ok := helperModes[mode]; ok {
		fn()
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
	os.Exit(2)
}

// Platform-specific test files may register additional helper modes.
var helperModes = map[string]func(){
	"lockholder":   helperLockHolder,
	"detachparent": helperDetachParent,
	"survivor":     helperSurvivor,
}

// startHelper re-executes the test binary in the selected helper mode.
func startHelper(t *testing.T, mode string, extraEnv ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = helperEnv(append([]string{"SEMANTICA_PLATFORM_HELPER=" + mode}, extraEnv...))
	return cmd
}

// helperEnv prevents nested helpers from inheriting stale helper state.
func helperEnv(pairs []string) []string {
	env := make([]string, 0, len(os.Environ())+len(pairs))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "SEMANTICA_PLATFORM_HELPER=") || strings.HasPrefix(kv, "SEMANTICA_HELPER_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, pairs...)
}

// helperLockHolder writes its data file while holding an exclusive lock.
func helperLockHolder() {
	lockPath := os.Getenv("SEMANTICA_HELPER_LOCK_PATH")
	dataPath := os.Getenv("SEMANTICA_HELPER_DATA_PATH")
	holdMS, err := strconv.Atoi(os.Getenv("SEMANTICA_HELPER_HOLD_MS"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad SEMANTICA_HELPER_HOLD_MS: %v\n", err)
		os.Exit(2)
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open lock file: %v\n", err)
		os.Exit(2)
	}
	if err := LockFile(f); err != nil {
		fmt.Fprintf(os.Stderr, "lock: %v\n", err)
		os.Exit(2)
	}
	fmt.Println("LOCKED")
	time.Sleep(time.Duration(holdMS) * time.Millisecond)
	if err := os.WriteFile(dataPath, []byte("written-under-lock"), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write data file: %v\n", err)
		os.Exit(2)
	}
	if err := UnlockFile(f); err != nil {
		fmt.Fprintf(os.Stderr, "unlock: %v\n", err)
		os.Exit(2)
	}
	_ = f.Close()
}

// helperDetachParent starts a detached helper and exits without waiting.
func helperDetachParent() {
	target := os.Getenv("SEMANTICA_HELPER_TARGET_FILE")
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = helperEnv([]string{
		"SEMANTICA_PLATFORM_HELPER=survivor",
		"SEMANTICA_HELPER_TARGET_FILE=" + target,
	})
	DetachProcess(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "spawn detached child: %v\n", err)
		os.Exit(2)
	}
	fmt.Println("SPAWNED")
}

// helperSurvivor writes its marker after its parent has exited.
func helperSurvivor() {
	target := os.Getenv("SEMANTICA_HELPER_TARGET_FILE")
	time.Sleep(500 * time.Millisecond)
	if err := os.WriteFile(target, []byte("alive-after-parent-exit"), 0o644); err != nil {
		os.Exit(2)
	}
}

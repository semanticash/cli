package util

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveExecutable_UsesPATHFirst(t *testing.T) {
	tmp := t.TempDir()
	tool := filepath.Join(tmp, "claude")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}

	got := resolveExecutable(
		[]string{"claude"},
		nil,
		func(name string) (string, error) {
			if name == "claude" {
				return tool, nil
			}
			return "", errors.New("not found")
		},
		func() (string, error) { return "", errors.New("unused") },
		func(string) string { return "" },
	)

	if got != tool {
		t.Fatalf("got %q, want %q", got, tool)
	}
}

func TestResolveExecutable_FallsBackToCommonDirs(t *testing.T) {
	home := t.TempDir()
	tool := filepath.Join(home, ".local", "bin", "gemini")
	if err := os.MkdirAll(filepath.Dir(tool), 0o755); err != nil {
		t.Fatalf("mkdir fallback dir: %v", err)
	}
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}

	got := resolveExecutable(
		[]string{"gemini"},
		nil,
		func(string) (string, error) { return "", errors.New("not found") },
		func() (string, error) { return home, nil },
		func(string) string { return "" },
	)

	if got != tool {
		t.Fatalf("got %q, want %q", got, tool)
	}
}

func TestResolveExecutable_UsesPackageManagerEnvDirs(t *testing.T) {
	prefix := t.TempDir()
	tool := filepath.Join(prefix, "bin", "copilot")
	if err := os.MkdirAll(filepath.Dir(tool), 0o755); err != nil {
		t.Fatalf("mkdir env dir: %v", err)
	}
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}

	got := resolveExecutable(
		[]string{"copilot"},
		nil,
		func(string) (string, error) { return "", errors.New("not found") },
		func() (string, error) { return "", errors.New("unused") },
		func(key string) string {
			if key == "NPM_CONFIG_PREFIX" {
				return prefix
			}
			return ""
		},
	)

	if got != tool {
		t.Fatalf("got %q, want %q", got, tool)
	}
}

func TestResolveExecutable_UsesExtraCandidates(t *testing.T) {
	tmp := t.TempDir()
	tool := filepath.Join(tmp, "agent")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write executable: %v", err)
	}

	got := resolveExecutable(
		[]string{"agent"},
		[]string{tool},
		func(string) (string, error) { return "", errors.New("not found") },
		func() (string, error) { return "", errors.New("unused") },
		func(string) string { return "" },
	)

	if got != tool {
		t.Fatalf("got %q, want %q", got, tool)
	}
}

func TestExecutableSearchDirs_DeduplicatesViaCaller(t *testing.T) {
	home := t.TempDir()
	dirs := executableSearchDirs(
		func() (string, error) { return home, nil },
		func(key string) string {
			switch key {
			case "NPM_CONFIG_PREFIX":
				return filepath.Join(home, ".local")
			case "PNPM_HOME":
				return filepath.Join(home, ".local", "bin")
			default:
				return ""
			}
		},
	)

	if len(dirs) == 0 {
		t.Fatal("expected search dirs")
	}
}

func TestIsExecutableFile_RequiresExecBitOnUnix(t *testing.T) {
	tmp := t.TempDir()
	tool := filepath.Join(tmp, "tool")
	mode := os.FileMode(0o755)
	if runtime.GOOS != "windows" {
		mode = 0o644
	}
	if err := os.WriteFile(tool, []byte("test"), mode); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got := isExecutableFile(tool)
	if runtime.GOOS == "windows" {
		if !got {
			t.Fatal("expected windows file to count as executable")
		}
		return
	}
	if got {
		t.Fatal("expected non-executable file to be rejected")
	}
}

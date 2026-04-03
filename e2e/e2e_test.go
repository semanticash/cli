//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var semBinary string // absolute path to compiled binary, set by TestMain

func TestMain(m *testing.M) {
	// Walk up from the test's working directory to find the repo root (go.mod).
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: getwd: %v\n", err)
		os.Exit(1)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			fmt.Fprintf(os.Stderr, "e2e: could not find go.mod (repo root)\n")
			os.Exit(1)
		}
		dir = parent
	}

	semBinary = filepath.Join(dir, "bin", "semantica")
	if _, err := os.Stat(semBinary); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: binary not found at %s - run 'make build' first\n", semBinary)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// initGitRepo creates a temp directory with a git repo, an initial commit,
// and environment isolation for Semantica.
func initGitRepo(t *testing.T) (dir string, env []string) {
	t.Helper()
	// Use os.MkdirTemp instead of t.TempDir() because the post-commit hook
	// spawns a background worker that may still be writing to .semantica/objects
	// when the test ends. t.TempDir() would fail the test on cleanup errors.
	raw, err := os.MkdirTemp("", "e2e-*")
	if err != nil {
		t.Fatal(err)
	}
	// Resolve symlinks so paths are consistent between hook subprocesses
	// (which use os.Getwd to resolve under /private/var/...) and test
	// commands (which pass --repo <tempdir>). This matters on macOS, where
	// /var resolves to /private/var.
	dir = raw
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	t.Cleanup(func() { os.RemoveAll(raw) })

	binDir := filepath.Dir(semBinary)
	env = []string{
		"SEMANTICA_HOME=" + filepath.Join(dir, ".semantica-global"),
		"HOME=" + dir,
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
	}

	runGit(t, env, dir, "init")
	runGit(t, env, dir, "config", "user.name", "E2E Test")
	runGit(t, env, dir, "config", "user.email", "e2e@test.local")

	// Initial commit so HEAD exists.
	commitFile(t, env, dir, "README.md", "# test repo\n", "initial commit")
	return dir, env
}

// runSem runs `semantica --repo <dir> <args...>` and returns stdout.
// Fails the test on non-zero exit.
func runSem(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	fullArgs := append([]string{"--repo", dir}, args...)
	return execSem(t, env, dir, nil, fullArgs...)
}

// runSemAllowFail runs semantica and returns stdout, stderr, and error
// without failing the test.
func runSemAllowFail(t *testing.T, env []string, dir string, args ...string) (string, string, error) {
	t.Helper()
	fullArgs := append([]string{"--repo", dir}, args...)
	cmd := exec.Command(semBinary, fullArgs...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// runSemRaw runs the binary with the given args (no --repo prefix).
// Used for commands like `worker run` that have their own --repo flag,
// and `capture` which doesn't use --repo.
func runSemRaw(t *testing.T, env []string, dir string, stdin []byte, args ...string) string {
	t.Helper()
	return execSem(t, env, dir, stdin, args...)
}

// execSem is the shared exec helper.
func execSem(t *testing.T, env []string, dir string, stdin []byte, args ...string) string {
	t.Helper()
	cmd := exec.Command(semBinary, args...)
	cmd.Dir = dir
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("semantica %s failed: %v\nstdout: %s\nstderr: %s",
			strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// runGit runs a git command in the given directory.
func runGit(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s failed: %v\nstderr: %s",
			strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

// commitFile writes a file, stages it, commits, and returns the HEAD hash.
func commitFile(t *testing.T, env []string, dir, filename, content, message string) string {
	t.Helper()
	path := filepath.Join(dir, filename)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", filename, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", filename, err)
	}
	runGit(t, env, dir, "add", filename)
	runGit(t, env, dir, "commit", "-m", message)
	return strings.TrimSpace(runGit(t, env, dir, "rev-parse", "HEAD"))
}

// enableRepo runs `semantica enable` in the given repo directory.
func enableRepo(t *testing.T, env []string, dir string) {
	t.Helper()
	runSem(t, env, dir, "enable")
}

// --- JSON output structs (subset of fields used by tests) ---

type listOutput struct {
	Count int              `json:"count"`
	Items []listCheckpoint `json:"items"`
}

type listCheckpoint struct {
	ID            string `json:"id"`
	CreatedAt     string `json:"created_at"`
	CreatedAtUnix int64  `json:"created_at_unix"`
	Kind          string `json:"kind"`
	Trigger       string `json:"trigger"`
	CommitHash    string `json:"commit_hash"`
	CommitSubject string `json:"commit_subject"`
	ManifestHash  string `json:"manifest_hash"`
}

type showOutput struct {
	RepoRoot     string `json:"repo_root"`
	CheckpointID string `json:"checkpoint_id"`
	CommitHash   string `json:"commit_hash"`
	ManifestHash string `json:"manifest_hash"`
	FileCount    int    `json:"file_count"`
	Kind         string `json:"kind"`
}

type explainOutput struct {
	CommitHash   string  `json:"commit_hash"`
	CheckpointID string  `json:"checkpoint_id"`
	FilesChanged int     `json:"files_changed"`
	LinesAdded   int     `json:"lines_added"`
	LinesDeleted int     `json:"lines_deleted"`
	AIPercentage float64 `json:"ai_percentage"`
}

type rewindOutput struct {
	RepoRoot           string `json:"repo_root"`
	CheckpointID       string `json:"checkpoint_id"`
	SafetyCheckpointID string `json:"safety_checkpoint_id"`
	FilesRestored      int    `json:"files_restored"`
	FilesDeleted       int    `json:"files_deleted"`
}

type sessionsOutput struct {
	Sessions []sessionInfo `json:"sessions"`
	Total    int           `json:"total"`
}

type sessionInfo struct {
	SessionID string `json:"session_id"`
	Provider  string `json:"provider"`
}

// latestCheckpointID returns the ID of the most recent checkpoint from `list --json`.
func latestCheckpointID(t *testing.T, env []string, dir string) string {
	t.Helper()
	out := runSem(t, env, dir, "list", "--json")
	var res listOutput
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse list output: %v\n%s", err, out)
	}
	if len(res.Items) == 0 {
		t.Fatal("no checkpoints found")
	}
	return res.Items[0].ID
}

// listCheckpoints returns the parsed list output.
func listCheckpoints(t *testing.T, env []string, dir string) listOutput {
	t.Helper()
	out := runSem(t, env, dir, "list", "--json", "-n", "100")
	var res listOutput
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse list output: %v\n%s", err, out)
	}
	return res
}

// runWorker runs the worker synchronously for the latest pending checkpoint.
func runWorker(t *testing.T, env []string, dir, commitHash string) {
	t.Helper()
	cpID := latestCheckpointID(t, env, dir)
	runSemRaw(t, env, dir, nil,
		"worker", "run", "--repo", dir, "--checkpoint", cpID, "--commit", commitHash)
}

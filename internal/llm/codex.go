package llm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/util"
)

// codexWriter wraps the OpenAI Codex CLI for use with the
// WriterRegistry fallback chain. The CLI ships as a single
// non-interactive binary; `codex exec` reads a prompt and runs the
// agent to completion. We use `--output-last-message <FILE>` to
// route the final narrative to a tempfile, which keeps the noisy
// progress/event stream off our return path and gives a clean
// final-answer channel to read on success.
type codexWriter struct{}

// Codex returns a Writer for the OpenAI Codex CLI. The composition
// root places it right after Claude Code in the production fallback
// order - Claude stays the primary daily driver; Codex is the
// obvious secondary now that we capture its sessions first-class.
func Codex() Writer { return codexWriter{} }

func (codexWriter) Name() string  { return "codex" }
func (codexWriter) Model() string { return "unknown" }

// Find resolves codex on PATH via exec.LookPath (cross-platform;
// honors .exe on Windows and the platform's PATH separator). No
// app-bundle fallback in v1 - the CLI ships via PATH on every host
// we support, and bundling internals are not a stable executable
// surface.
func (codexWriter) Find() string {
	return util.ResolveExecutable([]string{"codex"})
}

// Generate invokes `codex exec` with the prompt piped on stdin and
// the final narrative captured via `--output-last-message`. Runner
// contract (documented in plans/provider-registries-explicit-v1.md):
//
//   - stderr is always captured; on a non-zero exit it is included
//     in the returned error so callers can diagnose codex failures.
//   - The tempfile is read only after a successful exit. A failed
//     run may leave a partial / stale narrative written there;
//     reading it would surface garbage.
//   - The tempfile is removed on every exit path (success and
//     error) via defer so an early-return error doesn't leak the
//     file.
//   - Stdout is discarded because it carries codex's progress stream and
//     would otherwise leak noise into nothing useful.
func (codexWriter) Generate(ctx context.Context, binPath, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, llmShellTimeout)
	defer cancel()

	tmpFile, err := os.CreateTemp("", "semantica-codex-*.txt")
	if err != nil {
		return "", fmt.Errorf("create codex tempfile: %w", err)
	}
	tmpPath := tmpFile.Name()
	// Close immediately so codex can write to the path on every
	// platform (Windows file locking is finicky about a parent
	// process holding the handle open while a child writes).
	if closeErr := tmpFile.Close(); closeErr != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("close codex tempfile: %w", closeErr)
	}
	defer os.Remove(tmpPath)

	// --skip-git-repo-check is mandatory: we deliberately run from
	// os.TempDir() (cleanEnv strips GIT_* so the child process
	// can't discover the user's repo), but codex refuses to start
	// outside a git repo by default. The flag is documented in
	// `codex exec --help` as "Allow running Codex outside a Git
	// repository" and is the supported way to suppress that check.
	cmd := exec.CommandContext(ctx, binPath,
		"exec",
		"--skip-git-repo-check",
		"--output-last-message", tmpPath,
		"-",
	)
	platform.HideWindow(cmd)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Dir = os.TempDir()
	cmd.Env = cleanEnv(os.Environ())

	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Run(); err != nil {
		return "", formatShellError(ctx, err, &stderr, start)
	}

	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return "", fmt.Errorf("read codex narrative: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

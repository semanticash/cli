package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/redact"
	"github.com/semanticash/cli/internal/util"
)

const defaultModel = "sonnet"

// llmShellTimeout bounds provider shell-outs for detached background work.
// It leaves room for CLI startup and network latency without letting a stuck
// child process run indefinitely.
const llmShellTimeout = 120 * time.Second

// redactPrompt removes detected secrets from prompt content before it is sent
// to a provider.
func redactPrompt(prompt string) (string, error) {
	return redact.String(prompt)
}

// claudeCLIResponse represents the JSON response from claude --output-format json.
type claudeCLIResponse struct {
	Result string `json:"result"`
}

// GenerateResult holds the narrative plus metadata about how it was generated.
type GenerateResult struct {
	Narrative *NarrativeResult
	Provider  string
	Model     string
}

// GenerateTextResult holds a raw text response plus metadata.
type GenerateTextResult struct {
	Text     string
	Provider string
	Model    string
}

// llmProvider describes a CLI that can generate text.
type llmProvider struct {
	name  string
	model string
	find  func() string
	// runText sends a prompt and returns raw text.
	runText func(ctx context.Context, binPath, prompt string) (string, error)
}

// providers is the ordered fallback chain. First match wins.
var providers = []llmProvider{
	{
		name:    "claude_code",
		model:   defaultModel,
		find:    findClaude,
		runText: runClaude,
	},
	{
		name:    "cursor",
		model:   "unknown",
		find:    findCursorAgent,
		runText: runCursor,
	},
	{
		name:    "gemini_cli",
		model:   "unknown",
		find:    findGemini,
		runText: runGemini,
	},
	{
		name:    "copilot",
		model:   "unknown",
		find:    findCopilot,
		runText: runCopilot,
	},
}

// GenerateText sends a redacted prompt to an available AI CLI and returns raw
// text. Providers are tried in order until one succeeds.
func GenerateText(ctx context.Context, prompt string) (*GenerateTextResult, error) {
	redacted, err := redactPrompt(prompt)
	if err != nil {
		return nil, fmt.Errorf("egress redaction failed: %w", err)
	}
	prompt = redacted

	var lastErr error
	var tried int

	for _, p := range providers {
		binPath := p.find()
		if binPath == "" {
			continue
		}
		tried++

		text, err := p.runText(ctx, binPath, prompt)
		if err != nil {
			if lastErr != nil {
				lastErr = fmt.Errorf("%s: %w (after %v)", p.name, err, lastErr)
			} else {
				lastErr = fmt.Errorf("%s: %w", p.name, err)
			}
			continue
		}

		return &GenerateTextResult{
			Text:     text,
			Provider: p.name,
			Model:    p.model,
		}, nil
	}

	if lastErr != nil {
		return nil, fmt.Errorf("all providers failed: %w", lastErr)
	}
	return nil, fmt.Errorf("no AI CLI found. Install Claude Code, Cursor, Gemini CLI, or GitHub Copilot CLI")
}

// Generate sends a redacted prompt to an available AI CLI and returns a parsed
// narrative. Providers are tried in order until one succeeds.
func Generate(ctx context.Context, prompt string) (*GenerateResult, error) {
	redacted, err := redactPrompt(prompt)
	if err != nil {
		return nil, fmt.Errorf("egress redaction failed: %w", err)
	}
	prompt = redacted

	var lastErr error
	var tried int

	for _, p := range providers {
		binPath := p.find()
		if binPath == "" {
			continue
		}
		tried++

		text, err := p.runText(ctx, binPath, prompt)
		if err != nil {
			if lastErr != nil {
				lastErr = fmt.Errorf("%s: %w (after %v)", p.name, err, lastErr)
			} else {
				lastErr = fmt.Errorf("%s: %w", p.name, err)
			}
			continue
		}

		narrative, err := parseNarrativeJSON(text)
		if err != nil {
			if lastErr != nil {
				lastErr = fmt.Errorf("%s: %w (after %v)", p.name, err, lastErr)
			} else {
				lastErr = fmt.Errorf("%s: %w", p.name, err)
			}
			continue
		}

		return &GenerateResult{
			Narrative: narrative,
			Provider:  p.name,
			Model:     p.model,
		}, nil
	}

	if lastErr != nil {
		return nil, fmt.Errorf("all providers failed: %w", lastErr)
	}
	return nil, fmt.Errorf("no AI CLI found. Install Claude Code, Cursor, Gemini CLI, or GitHub Copilot CLI")
}

// --- Provider runners ---

// runClaude shells out to the Claude Code CLI.
func runClaude(ctx context.Context, claudePath, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, llmShellTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, claudePath,
		"--print",
		"--output-format", "json",
		"--model", defaultModel,
		"--setting-sources", "",
	)
	platform.HideWindow(cmd)
	cmd.Stdin = strings.NewReader(prompt)
	cmd.Dir = os.TempDir()
	cmd.Env = cleanEnv(os.Environ())

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Run(); err != nil {
		return "", formatShellError(ctx, err, &stderr, start)
	}

	var cliResp claudeCLIResponse
	if err := json.Unmarshal(stdout.Bytes(), &cliResp); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}

	return strings.TrimSpace(cliResp.Result), nil
}

// runCursor shells out to the Cursor CLI (agent binary).
func runCursor(ctx context.Context, agentPath, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, llmShellTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, agentPath,
		"-p",
		"--output-format", "text",
		prompt,
	)
	platform.HideWindow(cmd)
	cmd.Dir = os.TempDir()
	cmd.Env = cleanEnv(os.Environ())

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Run(); err != nil {
		return "", formatShellError(ctx, err, &stderr, start)
	}

	return strings.TrimSpace(stdout.String()), nil
}

// runGemini shells out to the Gemini CLI.
func runGemini(ctx context.Context, geminiPath, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, llmShellTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, geminiPath,
		"-p",
		prompt,
	)
	platform.HideWindow(cmd)
	cmd.Dir = os.TempDir()
	cmd.Env = cleanEnv(os.Environ())

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Run(); err != nil {
		return "", formatShellError(ctx, err, &stderr, start)
	}

	return strings.TrimSpace(stdout.String()), nil
}

// runCopilot shells out to the GitHub Copilot CLI.
func runCopilot(ctx context.Context, copilotPath, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, llmShellTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, copilotPath,
		"-p",
		prompt,
	)
	platform.HideWindow(cmd)
	cmd.Dir = os.TempDir()
	cmd.Env = cleanEnv(os.Environ())

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Run(); err != nil {
		return "", formatShellError(ctx, err, &stderr, start)
	}

	return strings.TrimSpace(stdout.String()), nil
}

// --- Provider discovery ---

func findClaude() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return util.ResolveExecutable([]string{"claude"})
	}
	return util.ResolveExecutable([]string{"claude"}, vsCodeClaudeBinaries(home)...)
}

// vsCodeClaudeBinaries returns paths to the Claude binary bundled inside
// VS Code (and VS Code Insiders) extensions, newest version first.
func vsCodeClaudeBinaries(home string) []string {
	bin := "claude"
	if runtime.GOOS == "windows" {
		bin = "claude.exe"
	}
	var candidates []string
	for _, dir := range []string{".vscode", ".vscode-insiders"} {
		pattern := filepath.Join(home, dir, "extensions", "anthropic.claude-code-*", "resources", "native-binary", bin)
		matches, err := filepath.Glob(pattern)
		if err != nil || len(matches) == 0 {
			continue
		}
		sort.Sort(sort.Reverse(sort.StringSlice(matches)))
		candidates = append(candidates, matches...)
	}
	return candidates
}

func findCursorAgent() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return util.ResolveExecutable([]string{"agent"})
	}
	return util.ResolveExecutable([]string{"agent"}, filepath.Join(home, ".cursor", "bin", "agent"))
}

func findGemini() string {
	return util.ResolveExecutable([]string{"gemini"})
}

func findCopilot() string {
	return util.ResolveExecutable([]string{"copilot"})
}

// --- Helpers ---

func fmtExecErr(err error, stderr *bytes.Buffer) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("exit %d: %s", exitErr.ExitCode(), stderr.String())
	}
	return fmt.Errorf("run: %w", err)
}

// formatShellError reports elapsed wall time for deadline errors and leaves
// other failures to fmtExecErr.
func formatShellError(ctx context.Context, err error, stderr *bytes.Buffer, start time.Time) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("timed out after %s", time.Since(start).Round(time.Second))
	}
	return fmtExecErr(err, stderr)
}

// cleanEnv removes environment variables that would cause the AI CLI
// subprocess to discover the user's repository or detect a nested session.
func cleanEnv(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		key := e[:strings.IndexByte(e, '=')]
		if strings.HasPrefix(key, "GIT_") || key == "CLAUDECODE" {
			continue
		}
		filtered = append(filtered, e)
	}
	return filtered
}

// parseNarrativeJSON extracts a NarrativeResult from a raw LLM response string.
// It strips markdown code block wrappers before unmarshalling.
func parseNarrativeJSON(raw string) (*NarrativeResult, error) {
	cleaned := extractJSONFromMarkdown(raw)
	var result NarrativeResult
	if err := json.Unmarshal([]byte(cleaned), &result); err != nil {
		return nil, fmt.Errorf("parse narrative JSON: %w (response: %s)", err, cleaned)
	}
	return &result, nil
}

// extractJSONFromMarkdown strips markdown code block wrappers if present.
func extractJSONFromMarkdown(s string) string {
	s = strings.TrimSpace(s)

	if strings.HasPrefix(s, "```json") {
		s = strings.TrimPrefix(s, "```json")
		if idx := strings.LastIndex(s, "```"); idx != -1 {
			s = s[:idx]
		}
		return strings.TrimSpace(s)
	}

	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if idx := strings.LastIndex(s, "```"); idx != -1 {
			s = s[:idx]
		}
		return strings.TrimSpace(s)
	}

	return s
}

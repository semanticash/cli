package providers

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/store/blobs"
	sqlstore "github.com/semanticash/cli/internal/store/sqlite"
	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
	"github.com/semanticash/cli/internal/toolsnap"
)

// These payloads exercise separate failure hooks and failures in ordinary results.
func TestShellFailureCompletionAdapters(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	transcript := filepath.Join(home, "projects", "repo", "session.jsonl")
	cases := []struct {
		provider, hook, body string
		explicitFailure      bool
	}{
		{"claude-code", "post-bash-failure", `{"session_id":"s","tool_name":"Bash","tool_use_id":"call-1","tool_input":{"command":"exit 1"},"error":"Exit code 1"}`, true},
		{"cursor", "post-tool-use-failure", `{"conversation_id":"s","tool_name":"Shell","tool_use_id":"call-1","tool_input":{"command":"exit 1"},"failure_type":"error","error_message":"Exit code 1"}`, true},
		{"copilot", "post-tool-use-failure", `{"sessionId":"s","timestamp":123,"toolName":"bash","toolArgs":{"command":"exit 1"},"error":"Exit code 1"}`, true},
		{"codex", "post-tool-use", `{"session_id":"s","tool_name":"Bash","tool_use_id":"call-1","tool_input":{"command":"exit 1"},"tool_response":"Process exited with code 1"}`, false},
		{"gemini-cli", "after-tool", `{"session_id":"s","tool_name":"run_shell_command","tool_input":{"command":"exit 1"},"tool_response":{"error":{"message":"Exit code 1"},"llmContent":"Command failed"}}`, false},
		{"kiro-cli", "post-tool-use", `{"session_id":"s","cwd":"/repo","tool_name":"execute_bash","tool_input":{"command":"exit 1"},"tool_response":{"exit_code":1}}`, false},
		{"copilot", "post-tool-use", `{"sessionId":"s","timestamp":123,"toolName":"bash","toolArgs":{"command":"exit 1"},"toolResult":{"resultType":"failure","textResultForLlm":"Exit code 1"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.provider+"/"+tc.hook, func(t *testing.T) {
			p := NewHookRegistry().Get(tc.provider)
			var payload map[string]any
			if err := json.Unmarshal([]byte(tc.body), &payload); err != nil {
				t.Fatal(err)
			}
			payload["transcript_path"] = transcript
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			ev, err := p.ParseHookEvent(context.Background(), tc.hook, strings.NewReader(string(body)))
			if err != nil || ev == nil {
				t.Fatalf("parse: event=%+v err=%v", ev, err)
			}
			if ev.Type != hooks.ToolStepCompleted || ev.ToolName != "Bash" || ev.ToolUseID == "" || ev.ToolFailed != tc.explicitFailure {
				t.Fatalf("failure did not normalize as terminal Bash: %+v", ev)
			}
			bs, err := blobs.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			events, err := p.(hooks.DirectHookEmitter).BuildHookEvents(context.Background(), ev, bs)
			if err != nil || len(events) != 1 {
				t.Fatalf("terminal lost before dispatch: events=%+v err=%v", events, err)
			}
		})
	}
}

func TestFailureHooksDoNotTurnEditInputsIntoChanges(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	for _, provider := range []string{"claude-code", "cursor", "copilot"} {
		t.Run(provider, func(t *testing.T) {
			payload := map[string]any{
				"session_id": "s", "sessionId": "s", "conversation_id": "s",
				"transcript_path": filepath.Join(home, "projects", "repo", "s.jsonl"),
				"tool_name":       "Write", "toolName": "create", "tool_use_id": "call-1",
				"tool_input": map[string]string{"file_path": "/repo/f.txt", "content": "never written"},
				"toolArgs":   map[string]string{"path": "/repo/f.txt", "file_text": "never written"},
			}
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			hook := "post-tool-use-failure"
			if provider == "claude-code" {
				hook = "post-bash-failure"
			}
			ev, err := NewHookRegistry().Get(provider).ParseHookEvent(context.Background(), hook, strings.NewReader(string(body)))
			if err != nil || ev != nil {
				t.Fatalf("failed edit became evidence: %+v, %v", ev, err)
			}
		})
	}
}

func TestFailedShellDoesNotBlockGroupEvidence(t *testing.T) {
	for _, provider := range []string{"claude-code", "codex", "cursor"} {
		for _, failedLast := range []bool{false, true} {
			name := provider + "/failure-first"
			if failedLast {
				name = provider + "/failure-last"
			}
			t.Run(name, func(t *testing.T) {
				t.Cleanup(hooks.SetToolWindowDeadlineForTest(30 * time.Second))
				home := t.TempDir()
				t.Setenv("SEMANTICA_HOME", home)
				t.Setenv("CLAUDE_CONFIG_DIR", home)
				t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
				t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
				ctx := context.Background()
				repo, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				git := func(args ...string) {
					t.Helper()
					cmd := exec.Command("git", args...)
					cmd.Dir = repo
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("git: %v: %s", err, out)
					}
				}
				git("init", "-q")
				git("-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "core.hooksPath="+home, "commit", "-qm", "baseline", "--allow-empty")
				semDir := filepath.Join(repo, ".semantica")
				if err := os.MkdirAll(semDir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(semDir, "enabled"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				dbPath := filepath.Join(semDir, "lineage.db")
				if err := sqlstore.MigratePath(ctx, dbPath); err != nil {
					t.Fatal(err)
				}
				db, err := sqlstore.Open(ctx, dbPath, sqlstore.DefaultOpenOptions())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = sqlstore.Close(db) }()
				if err := db.Queries.InsertRepository(ctx, sqldb.InsertRepositoryParams{RepositoryID: uuid.NewString(), RootPath: repo, CreatedAt: 1, EnabledAt: 1}); err != nil {
					t.Fatal(err)
				}
				bh, err := broker.Open(ctx, filepath.Join(home, "repos.json"))
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = broker.Close(bh) }()
				if err := broker.Register(ctx, bh, repo, repo); err != nil {
					t.Fatal(err)
				}
				objects, err := broker.GlobalObjectsDir()
				if err != nil {
					t.Fatal(err)
				}
				bs, err := blobs.NewStore(objects)
				if err != nil {
					t.Fatal(err)
				}
				if err := hooks.SaveCaptureState(&hooks.CaptureState{SessionID: "s", Provider: provider, TurnID: "t", CWD: repo, Timestamp: 1}); err != nil {
					t.Fatal(err)
				}
				p := NewHookRegistry().Get(provider)
				dispatch := func(id string, pre, failed bool) {
					t.Helper()
					hook, tool := "post-tool-use", "Bash"
					if pre {
						hook = "pre-tool-use"
					}
					switch provider {
					case "claude-code":
						hook = "post-bash"
						if pre {
							hook = "pre-bash"
						} else if failed {
							hook = "post-bash-failure"
						}
					case "cursor":
						tool = "Shell"
						if !pre && failed {
							hook = "post-tool-use-failure"
						}
					}
					payload := map[string]any{"session_id": "s", "conversation_id": "s", "cwd": repo,
						"transcript_path": filepath.Join(home, "projects", "repo", "s.jsonl"),
						"tool_name":       tool, "tool_use_id": id, "tool_input": map[string]string{"command": "generate fixtures"},
						"tool_response": map[string]any{"exit_code": 0}, "tool_output": "ok"}
					if failed {
						payload["error"], payload["failure_type"] = "Exit code 1", "error"
						payload["tool_response"] = "Process exited with code 1"
					}
					body, err := json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					ev, err := p.ParseHookEvent(ctx, hook, strings.NewReader(string(body)))
					if err != nil || ev == nil {
						t.Fatalf("parse %s: %+v %v", hook, ev, err)
					}
					if err := hooks.Dispatch(ctx, p, ev, bh, bs); err != nil {
						t.Fatal(err)
					}
				}
				dispatch("failed", true, false)
				dispatch("successful", true, false)
				for _, file := range []string{"before-error.txt", "successful.txt"} {
					if err := os.WriteFile(filepath.Join(repo, file), []byte(file+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if failedLast {
					dispatch("successful", false, false)
					dispatch("failed", false, true)
				} else {
					dispatch("failed", false, true)
					dispatch("successful", false, false)
				}
				dispatch("failed", false, true)
				reg, err := toolsnap.OpenRegistry(semDir)
				if err != nil {
					t.Fatal(err)
				}
				pending, err := reg.Stale(ctx, time.Now().Add(time.Hour).UnixMilli())
				if err != nil || len(pending) != 0 {
					t.Fatalf("windows remain: %+v, %v", pending, err)
				}
				rows, err := db.DB.QueryContext(ctx, "SELECT evidence_hash FROM agent_event_evidence_links WHERE evidence_kind = 'tool_delta'")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = rows.Close() }()
				repoBlobs, err := blobs.NewStore(filepath.Join(semDir, "objects"))
				if err != nil {
					t.Fatal(err)
				}
				count := 0
				for rows.Next() {
					var hash string
					if err := rows.Scan(&hash); err != nil {
						t.Fatal(err)
					}
					raw, err := repoBlobs.Get(ctx, hash)
					if err != nil {
						t.Fatal(err)
					}
					delta, err := toolsnap.ParseDelta(raw)
					if err != nil || delta.Status != "complete" {
						t.Fatalf("delta: %+v %v", delta, err)
					}
					files := map[string]bool{}
					for _, file := range delta.Files {
						files[file.Path] = true
					}
					if !files["before-error.txt"] || !files["successful.txt"] {
						t.Fatalf("missing writes: %+v", delta.Files)
					}
					count++
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				if count != 2 {
					t.Fatalf("evidence links = %d, want 2", count)
				}
			})
		}
	}
}

//go:build turnexperiment

package toolsnap

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type completionSignal struct {
	Source       string          `json:"source"`
	Name         string          `json:"name"`
	At           time.Time       `json:"at"`
	FixtureState string          `json:"fixture_state"` // Last reported state, not a liveness probe.
	Data         json.RawMessage `json:"data,omitempty"`
}

// Fixture barriers establish ground truth only. They never produce provider evidence.
type completionRecorder struct {
	mu                                   sync.Mutex
	state                                string
	signals                              []completionSignal
	started, released, completed         chan struct{}
	startOnce, releaseOnce, completeOnce sync.Once
}

func newCompletionRecorder() *completionRecorder {
	return &completionRecorder{state: "not_started", started: make(chan struct{}), released: make(chan struct{}), completed: make(chan struct{})}
}

func (r *completionRecorder) record(source, name string, data json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.signals = append(r.signals, completionSignal{source, name, time.Now(), r.state, append(json.RawMessage(nil), data...)})
}

func (r *completionRecorder) release(reason string) {
	r.releaseOnce.Do(func() { r.record("fixture_control", reason, nil); close(r.released) })
}

func (r *completionRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.URL.Path {
	case "/fixture/started":
		r.mu.Lock()
		r.state = "running"
		r.mu.Unlock()
		r.record("fixture", "child_started", nil)
		r.startOnce.Do(func() { close(r.started) })
	case "/fixture/completed":
		var result json.RawMessage
		if err := json.NewDecoder(req.Body).Decode(&result); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		r.mu.Lock()
		r.state = "completed"
		r.mu.Unlock()
		r.record("fixture", "child_wait_returned", result)
		r.completeOnce.Do(func() { close(r.completed) })
	case "/fixture/started-barrier":
		select {
		case <-r.started:
		case <-req.Context().Done():
		}
	case "/fixture/release":
		if req.Method == "POST" {
			r.release("explicit_fixture_release")
			return
		}
		select {
		case <-r.released:
		case <-req.Context().Done():
		}
	case "/hook":
		var message struct {
			Raw json.RawMessage `json:"raw"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<20)).Decode(&message); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var envelope struct {
			Name string `json:"hook_event_name"`
		}
		if err := json.Unmarshal(message.Raw, &envelope); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		r.record("provider_hook", envelope.Name, message.Raw)
		// This stimulus ends the controlled workload after recording Stop's state.
		// It does not delay Stop or tell the provider that completion was established.
		if envelope.Name == "Stop" {
			r.release("release_after_recorded_stop")
		}
	default:
		http.NotFound(w, req)
	}
}

const completionFixture = `import json, subprocess, sys, urllib.request
endpoint, mode = sys.argv[1:3]
def post(name, value=None):
    request = urllib.request.Request(endpoint + "/fixture/" + name,
        data=json.dumps(value).encode(), headers={"Content-Type":"application/json"})
    with urllib.request.urlopen(request, timeout=150) as response:
        response.read()
def barrier(name):
    with urllib.request.urlopen(endpoint + "/fixture/" + name, timeout=150) as response:
        response.read()
if mode == "release":
    post("release")
elif mode == "worker":
    post("started")
    if sys.argv[3] != "foreground":
        barrier("release")
elif mode == "detached":
    subprocess.Popen([sys.executable, __file__, endpoint, "supervisor", "managed"],
        start_new_session=True, stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    barrier("started-barrier")
else:
    behavior = sys.argv[3] if mode == "supervisor" else mode
    child = subprocess.run([sys.executable, __file__, endpoint, "worker", behavior])
    post("completed", {"returncode":child.returncode})
    print("controlled child exited", child.returncode, flush=True)
`

func TestTurnExperimentCompletionRecorder(t *testing.T) {
	r := newCompletionRecorder()
	r.state = "running"
	request := httptest.NewRequest("POST", "/hook", strings.NewReader(`{"raw":{"hook_event_name":"Stop","background_tasks":[]}}`))
	r.ServeHTTP(httptest.NewRecorder(), request)
	if r.state != "running" || len(r.signals) != 2 || r.signals[0].FixtureState != "running" {
		t.Fatal("Stop fabricated completion")
	}
	select {
	case <-r.released:
	default:
		t.Fatal("fixture not released")
	}
	select {
	case <-r.completed:
		t.Fatal("fixture completion fabricated")
	default:
	}
	if !strings.Contains(string(r.signals[0].Data), "background_tasks") {
		t.Fatal("provider fields lost")
	}
}

// TestTurnExperimentCompletionSignals observes real providers without snapshots.
// Only the fixture knows its process tree; no process discovery is performed.
func TestTurnExperimentCompletionSignals(t *testing.T) {
	provider := os.Getenv("SEMANTICA_COMPLETION_PROVIDER")
	if provider == "" {
		t.Skip("set SEMANTICA_COMPLETION_PROVIDER to codex or claude")
	}
	if provider != "codex" && provider != "claude" {
		t.Fatal("unknown provider")
	}
	output := os.Getenv("SEMANTICA_COMPLETION_OUTPUT")
	if output == "" {
		t.Fatal("SEMANTICA_COMPLETION_OUTPUT required")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{"foreground", "managed", "detached", "managed_join"}
	if name := os.Getenv("SEMANTICA_COMPLETION_CASE"); name != "" {
		cases = []string{name}
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			r := newCompletionRecorder()
			server := httptest.NewServer(r)
			defer server.Close()
			defer r.release("cleanup_release")
			fixture := filepath.Join(dir, "fixture.py")
			if err := os.WriteFile(fixture, []byte(completionFixture), 0o600); err != nil {
				t.Fatal(err)
			}
			command := "TURN_HOOK_ENDPOINT=" + lifecycleQuote(server.URL+"/hook") + " TURN_HOOK_PROVIDER=" + provider + " " + lifecycleQuote(binary) + " -test.run '^TestTurnExperimentHook$' >>" + lifecycleQuote(filepath.Join(dir, "hook.log")) + " 2>&1"
			events := []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop", "SessionEnd"}
			if provider == "claude" {
				events = append(events, "PostToolUseFailure", "Notification", "TaskCreated", "TaskCompleted", "StopFailure", "SubagentStart", "SubagentStop")
			}
			hookMap := map[string]any{}
			var toml strings.Builder
			toml.WriteString("hooks={")
			for i, event := range events {
				timeout := 30
				if event == "SessionEnd" {
					timeout = 3
				}
				hookMap[event] = []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": timeout}}}}
				if i > 0 {
					toml.WriteString(",")
				}
				encoded, _ := json.Marshal(command)
				fmt.Fprintf(&toml, "%s=[{hooks=[{type=\"command\",command=%s,timeout=%d}]}]", event, encoded, timeout)
			}
			toml.WriteString("}")
			settings, _ := json.Marshal(map[string]any{"hooks": hookMap})
			settingsPath := filepath.Join(dir, "settings.json")
			if err := os.WriteFile(settingsPath, settings, 0o600); err != nil {
				t.Fatal(err)
			}
			mode := name
			if name == "managed_join" {
				mode = "managed"
			}
			launch := lifecycleQuote(python) + " " + lifecycleQuote(fixture) + " " + lifecycleQuote(server.URL) + " " + mode
			release := lifecycleQuote(python) + " " + lifecycleQuote(fixture) + " " + lifecycleQuote(server.URL) + " release"
			prompt := "This is an authorized capability experiment in a temporary directory. Do not inspect or modify files, use networking except the fixture's local endpoint, or launch unrelated commands. Run exactly this command once: " + launch + ". "
			if name == "managed" || name == "managed_join" {
				if provider == "claude" {
					prompt += "Use Bash with run_in_background=true. "
				} else {
					prompt += "Use exec_command with yield_time_ms=1000 to obtain a running execution session. Do not shell-background the command. "
				}
				if name == "managed_join" {
					prompt += "Then run this separate foreground release command: " + release + ". Then use your native TaskOutput or write_stdin tool to wait for the ORIGINAL background command's terminal result. Do not inspect process state. After observing that terminal result, reply done."
				} else {
					prompt += "Do not poll, wait, or use TaskOutput/write_stdin after the initial launch returns. Reply launched immediately and stop. The fixture will be released externally after Stop."
				}
			} else {
				prompt += "Use normal foreground Bash, no background flag, and wait only for this command to return. Do not inspect its implementation or processes. Then reply done and stop."
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			var args []string
			if provider == "claude" {
				args = []string{"-p", "--setting-sources", "", "--settings", settingsPath, "--strict-mcp-config", "--mcp-config", "{\"mcpServers\":{}}", "--no-session-persistence", "--tools", "Bash,TaskOutput", "--allowedTools", "Bash,TaskOutput", "--output-format", "stream-json", "--verbose", prompt}
			} else {
				args = []string{"exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--skip-git-repo-check", "--enable", "hooks", "--dangerously-bypass-hook-trust", "--approve-for-me", "-c", "sandbox_workspace_write.network_access=true", "-c", toml.String(), "--json", prompt}
			}
			cmd := exec.CommandContext(ctx, provider, args...)
			cmd.Dir = dir
			log, err := os.Create(output + "." + name + ".log")
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = log
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			scan := bufio.NewScanner(stdout)
			scan.Buffer(make([]byte, 64<<10), 4<<20)
			for scan.Scan() {
				line := append([]byte(nil), scan.Bytes()...)
				_, _ = log.Write(append(line, '\n'))
				if json.Valid(line) {
					var event map[string]any
					_ = json.Unmarshal(line, &event)
					r.record("provider_stream", fmt.Sprint(event["type"]), line)
				}
			}
			processErr := cmd.Wait()
			r.record("harness", "provider_exited", nil)
			r.release("release_after_provider_exit")
			// Await the fixture's own wait result, never a guessed completion interval.
			observationError := ""
			select {
			case <-r.started:
				select {
				case <-r.completed:
				case <-ctx.Done():
					observationError = "fixture_exit_not_received_before_test_deadline"
				}
			default:
				observationError = "fixture_start_not_observed"
			}
			server.Close()
			r.mu.Lock()
			data, err := json.MarshalIndent(struct {
				Provider, Case, ProcessError, FixtureState string
				ObservationError                           string
				Signals                                    []completionSignal
			}{provider, name, fmt.Sprint(processErr), r.state, observationError, r.signals}, "", "  ")
			r.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(output+"."+name+".json", data, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Logf("provider=%s case=%s fixture=%s process=%v", provider, name, r.state, processErr)
			if scan.Err() != nil {
				t.Error(scan.Err())
			}
		})
	}
}

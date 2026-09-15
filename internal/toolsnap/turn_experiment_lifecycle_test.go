//go:build turnexperiment

package toolsnap

import (
	"bytes"
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

type lifecycleMessage struct {
	Raw struct {
		Name     string          `json:"hook_event_name"`
		Session  string          `json:"session_id"`
		Turn     string          `json:"turn_id"`
		Tool     string          `json:"tool_name"`
		Input    json.RawMessage `json:"tool_input"`
		Response json.RawMessage `json:"tool_response"`
	} `json:"raw"`
	Event *struct {
		Type      int
		SessionID string
	} `json:"event"`
}

type lifecycleTrace struct {
	Name     string           `json:"name"`
	Arrived  time.Time        `json:"arrived"`
	Finished time.Time        `json:"finished"`
	Message  lifecycleMessage `json:"message"`
}

// One isolated CLI invocation owns one turn. No missing boundary is synthesized.
type lifecycleReceiver struct {
	mu                    sync.Mutex
	subjects              []turnSubject
	storage               string
	turn                  *observedTurn
	session, providerTurn string
	starts, ends          int
	baselineDone, endDone time.Time
	issues                []string
	traces                []lifecycleTrace
	background            bool
	backgroundProbe       func() bool
	backgroundAliveProbe  func() bool
	backgroundAliveAtStop bool
}

func (r *lifecycleReceiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	arrived := time.Now()
	var m lifecycleMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<20)).Decode(&m); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	defer func() { r.traces = append(r.traces, lifecycleTrace{m.Raw.Name, arrived, time.Now(), m}) }()
	ctx, cancel := context.WithTimeout(req.Context(), time.Minute)
	defer cancel()
	switch m.Raw.Name {
	case "UserPromptSubmit":
		r.starts++
		if r.starts != 1 || r.ends != 0 || len(r.traces) != 0 {
			r.issues = append(r.issues, "duplicate_or_late_start")
			return
		}
		if m.Event == nil || m.Event.Type != 0 || m.Event.SessionID == "" || m.Event.SessionID != m.Raw.Session {
			r.issues = append(r.issues, "invalid_start_adapter_identity")
			return
		}
		r.session, r.providerTurn = m.Raw.Session, m.Raw.Turn
		var err error
		r.turn, err = beginObservedTurn(ctx, r.session, "isolated-invocation", r.storage, r.subjects, len(r.subjects))
		if err != nil {
			r.issues = append(r.issues, err.Error())
			return
		}
		r.baselineDone = time.Now()
	case "Stop":
		r.ends++
		if r.ends != 1 || r.turn == nil || r.baselineDone.IsZero() || arrived.Before(r.baselineDone) {
			r.issues = append(r.issues, "duplicate_missing_or_early_end")
			return
		}
		if m.Event == nil || m.Event.Type != 1 || m.Event.SessionID != r.session || m.Raw.Session != r.session || (r.providerTurn != "" && m.Raw.Turn != r.providerTurn) {
			r.issues = append(r.issues, "end_identity_mismatch")
			return
		}
		if r.backgroundProbe != nil && r.backgroundProbe() {
			r.background = true
		}
		if r.backgroundAliveProbe != nil {
			r.backgroundAliveAtStop = r.backgroundAliveProbe()
		}
		finishObservedTurn(ctx, r.turn, r.session, "isolated-invocation", !r.background && len(r.issues) == 0)
		r.endDone = time.Now()
	default:
		if r.baselineDone.IsZero() || arrived.Before(r.baselineDone) || r.ends != 0 {
			r.issues = append(r.issues, "activity_outside_boundaries")
		}
		if m.Raw.Session != r.session {
			r.issues = append(r.issues, "activity_session_mismatch")
		}
		// A background request or returned execution session prevents a quiescence claim.
		var input map[string]any
		var response map[string]any
		_ = json.Unmarshal(m.Raw.Input, &input)
		_ = json.Unmarshal(m.Raw.Response, &response)
		if input["run_in_background"] == true || response["session_id"] != nil || response["backgroundTaskId"] != nil || strings.Contains(string(m.Raw.Response), "Process running with session ID") {
			r.background = true
		}
	}
}

func (r *lifecycleReceiver) result() ([]turnSample, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	issues := append([]string(nil), r.issues...)
	if r.starts != 1 || r.baselineDone.IsZero() {
		issues = append(issues, "missing_baseline")
	}
	if r.ends != 1 || r.endDone.IsZero() {
		issues = append(issues, "missing_reconciliation")
	}
	if r.background {
		issues = append(issues, "background_quiescence_unproven")
	}
	var samples []turnSample
	if r.turn != nil {
		samples = append(samples, r.turn.Samples...)
	} else {
		for _, subject := range r.subjects {
			samples = append(samples, turnSample{turnSubject: subject})
		}
	}
	if len(issues) != 0 {
		for i := range samples {
			samples[i].State = "unknown"
			samples[i].Reason = strings.Join(issues, ";")
			samples[i].Files = nil
			samples[i].Boundaries = nil
		}
	}
	return samples, issues
}

func lifecycleQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func TestTurnExperimentBoundaryFailures(t *testing.T) {
	for _, sequence := range [][]string{
		{"Stop"}, {"UserPromptSubmit"}, {"PreToolUse", "UserPromptSubmit", "Stop"},
		{"UserPromptSubmit", "UserPromptSubmit", "Stop"},
		{"UserPromptSubmit", "Stop", "Stop"},
		{"UserPromptSubmit", "Stop", "PreToolUse"},
		{"UserPromptSubmit", "background", "Stop"},
		{"UserPromptSubmit", "wrong_session"},
	} {
		t.Run(strings.Join(sequence, "_"), func(t *testing.T) {
			r := &lifecycleReceiver{subjects: turnRepos(t), storage: t.TempDir()}
			for _, name := range sequence {
				sendLifecycleTest(t, r, name)
			}
			samples, issues := r.result()
			if len(issues) == 0 {
				t.Fatal("invalid boundary accepted")
			}
			for _, s := range samples {
				if s.State != "unknown" {
					t.Fatalf("unsafe result: %+v", s)
				}
			}
		})
	}
}

func sendLifecycleTest(t *testing.T, r *lifecycleReceiver, name string) {
	t.Helper()
	m := lifecycleMessage{}
	m.Raw.Name, m.Raw.Session = name, "session"
	if name == "background" {
		m.Raw.Name = "PreToolUse"
		m.Raw.Input = json.RawMessage(`{"run_in_background":true}`)
	}
	if name == "wrong_session" {
		m.Raw.Name, m.Raw.Session = "Stop", "other"
	}
	if m.Raw.Name == "UserPromptSubmit" || m.Raw.Name == "Stop" {
		m.Event = &struct {
			Type      int
			SessionID string
		}{SessionID: m.Raw.Session}
		if m.Raw.Name == "Stop" {
			m.Event.Type = 1
		}
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", bytes.NewReader(data)))
}

func TestTurnExperimentBoundaryChanges(t *testing.T) {
	r := &lifecycleReceiver{subjects: turnRepos(t), storage: t.TempDir()}
	sendLifecycleTest(t, r, "UserPromptSubmit")
	if r.baselineDone.IsZero() {
		t.Fatal("start returned before baseline")
	}
	sendLifecycleTest(t, r, "PreToolUse")
	writeFile(t, r.subjects[1].Path, "a.txt", "cross repository\n")
	sendLifecycleTest(t, r, "PostToolUse")
	sendLifecycleTest(t, r, "Stop")
	if r.endDone.IsZero() {
		t.Fatal("end returned before reconciliation")
	}
	samples, issues := r.result()
	if len(issues) != 0 || samples[0].State != "unchanged" || samples[1].State != "changed" || samples[2].State != "unchanged" {
		t.Fatalf("samples=%+v issues=%v", samples, issues)
	}
}

// TestTurnExperimentLifecycle installs only invocation-local hooks. Live writes
// are confined to the owned fixtures in sample and sample2 on detached HEADs.
func TestTurnExperimentLifecycle(t *testing.T) {
	provider := os.Getenv("SEMANTICA_TURN_EXPERIMENT_PROVIDER")
	if provider == "" {
		t.Skip("set SEMANTICA_TURN_EXPERIMENT_PROVIDER to codex or claude")
	}
	if provider != "codex" && provider != "claude" {
		t.Fatal("unknown provider")
	}
	raw, err := os.ReadFile(os.Getenv("SEMANTICA_TURN_EXPERIMENT_REPOS"))
	if err != nil {
		t.Fatal(err)
	}
	var subjects []turnSubject
	if err := json.Unmarshal(raw, &subjects); err != nil {
		t.Fatal(err)
	}
	var a, b string
	for _, s := range subjects {
		if filepath.Base(s.Path) == "sample" {
			a = s.Path
		}
		if filepath.Base(s.Path) == "sample2" {
			b = s.Path
		}
	}
	if len(subjects) != 7 || a == "" || b == "" {
		t.Fatal("expected seven repositories including sample and sample2")
	}
	prepareTurnWorkload(t, a)
	prepareTurnWorkload(t, b)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output := os.Getenv("SEMANTICA_TURN_EXPERIMENT_OUTPUT")
	if output == "" {
		t.Fatal("diagnostic output path required")
	}
	storage := t.TempDir()
	type report struct {
		Provider, Case, ProcessError  string
		BackgroundAliveAtStop         bool
		Start, End                    time.Duration
		BaselineDone, EndDone, Exited time.Time
		Issues                        []string
		Traces                        []lifecycleTrace
		Samples                       []turnSample
		Expected                      []string
	}
	var reports []report
	cases := []string{"single", "opaque", "edit", "commit", "commit_dirty", "unchanged"}
	if selected := os.Getenv("SEMANTICA_TURN_EXPERIMENT_CASE"); selected != "" {
		cases = []string{selected}
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			resetTurnWorkload(t, a)
			resetTurnWorkload(t, b)
			r := &lifecycleReceiver{subjects: append([]turnSubject(nil), subjects...), storage: storage}
			server := httptest.NewServer(r)
			defer server.Close()
			dir := t.TempDir()
			command := "TURN_HOOK_ENDPOINT=" + lifecycleQuote(server.URL) + " TURN_HOOK_PROVIDER=" + provider + " " + lifecycleQuote(binary) + " -test.run '^TestTurnExperimentHook$' >" + lifecycleQuote(filepath.Join(dir, "hook.log")) + " 2>&1"
			hookMap := map[string]any{}
			for _, event := range []string{"UserPromptSubmit", "Stop", "PreToolUse", "PostToolUse"} {
				hookMap[event] = []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 90}}}}
			}
			settings, _ := json.Marshal(map[string]any{"hooks": hookMap})
			settingsPath := filepath.Join(dir, "settings.json")
			if err := os.WriteFile(settingsPath, settings, 0o600); err != nil {
				t.Fatal(err)
			}
			fileA := filepath.Join(a, turnWorkloadDir, "file00.txt")
			fileB := filepath.Join(b, turnWorkloadDir, "file00.txt")
			marker := "lifecycle-" + provider + "-" + name
			instruction := ""
			expected := []string{}
			switch name {
			case "single":
				instruction = fmt.Sprintf("Use your file editing tool to append a line %q to %s.", marker, fileA)
				expected = []string{a}
			case "opaque":
				script := filepath.Join(dir, "generate.sh")
				if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' "+lifecycleQuote(marker)+" >> "+lifecycleQuote(fileB)+"\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				instruction = "Run this opaque generator exactly once using Bash: sh " + lifecycleQuote(script) + ". Do not inspect its implementation."
				expected = []string{b}
			case "edit":
				instruction = fmt.Sprintf("Use your file editing tool (not Bash) to append a line %q to %s.", marker, fileB)
				expected = []string{b}
			case "commit", "commit_dirty":
				instruction = fmt.Sprintf("Append a line %q to %s, then commit ONLY that file using git -c core.hooksPath=/dev/null -c core.fsmonitor=false -c commit.gpgsign=false -C %s commit --only -qm 'lifecycle experiment' -- %s.", marker, fileB, lifecycleQuote(b), lifecycleQuote(filepath.Join(turnWorkloadDir, "file00.txt")))
				if name == "commit_dirty" {
					instruction += " After the commit, append another line 'after-commit' to " + filepath.Join(b, turnWorkloadDir, "file01.txt") + "; leave it uncommitted."
				}
				expected = []string{b}
			case "unchanged":
				instruction = "Make no tool calls and no file changes. Reply done."
			case "background":
				ready, stop, done := filepath.Join(dir, "ready"), filepath.Join(dir, "stop"), filepath.Join(dir, "done")
				pidFile := filepath.Join(dir, "pid")
				script := filepath.Join(dir, "background.sh")
				// The child touches only test markers and exits on request or after 60s.
				body := "#!/bin/sh\n( touch " + lifecycleQuote(ready) + "; i=0; while [ ! -f " + lifecycleQuote(stop) + " ] && [ \"$i\" -lt 600 ]; do sleep 0.1; i=$((i+1)); done; touch " + lifecycleQuote(done) + " ) </dev/null >/dev/null 2>&1 &\necho $! > " + lifecycleQuote(pidFile) + "\nwhile [ ! -f " + lifecycleQuote(ready) + " ]; do sleep 0.01; done\n"
				if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
				r.backgroundProbe = func() bool {
					_, readyErr := os.Stat(ready)
					_, doneErr := os.Stat(done)
					return readyErr == nil && os.IsNotExist(doneErr)
				}
				r.backgroundAliveProbe = func() bool {
					pid, err := os.ReadFile(pidFile)
					if err != nil {
						return false
					}
					out, err := exec.Command("ps", "-p", strings.TrimSpace(string(pid)), "-o", "stat=").Output()
					state := strings.TrimSpace(string(out))
					return err == nil && state != "" && !strings.HasPrefix(state, "Z")
				}
				t.Cleanup(func() {
					if err := os.WriteFile(stop, nil, 0o600); err != nil {
						t.Error(err)
						return
					}
					deadline := time.Now().Add(3 * time.Second)
					for r.backgroundAliveProbe() && time.Now().Before(deadline) {
						time.Sleep(20 * time.Millisecond)
					}
					if r.backgroundAliveProbe() {
						t.Error("owned background child did not stop")
					}
				})
				instruction = "For this background-boundary control, launch this authorized test script once with Bash: sh " + lifecycleQuote(script) + ". It deliberately leaves a marker-only background child running; do not wait for that child. Reply done immediately after the launch command returns. The test harness stops the child. Do not edit repository files."
			default:
				t.Fatal("unknown case")
			}
			prompt := "This is an authorized, isolated lifecycle test. Modify only the explicitly named files under semantica_turn_benchmark. Do not modify configuration, inspect secrets, invoke Semantica, push, or switch branches. Do not change any other file. Launch no background work except the explicitly requested background control. Complete the exact task and stop. " + instruction
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			var args []string
			if provider == "claude" {
				args = []string{"-p", "--setting-sources", "", "--settings", settingsPath, "--strict-mcp-config", "--mcp-config", "{\"mcpServers\":{}}", "--no-session-persistence", "--tools", "Bash,Read,Write,Edit", "--allowedTools", "Bash,Read,Write,Edit", "--add-dir", b, "--output-format", "stream-json", "--verbose", prompt}
			} else {
				// CLI overrides are TOML; JSON objects are not TOML inline tables.
				var config strings.Builder
				config.WriteString("hooks={")
				for i, event := range []string{"UserPromptSubmit", "Stop", "PreToolUse", "PostToolUse"} {
					if i > 0 {
						config.WriteString(",")
					}
					encoded, _ := json.Marshal(command)
					fmt.Fprintf(&config, "%s=[{hooks=[{type=\"command\",command=%s,timeout=90}]}]", event, encoded)
				}
				config.WriteString("}")
				args = []string{"exec", "--ignore-user-config", "--ignore-rules", "--ephemeral", "--enable", "hooks", "--dangerously-bypass-hook-trust", "--approve-for-me", "--add-dir", b, "--add-dir", dir, "-c", config.String(), "--json", prompt}
			}
			cmd := exec.CommandContext(ctx, provider, args...)
			cmd.Dir = a
			cmd.Env = os.Environ()
			log, err := os.Create(output + "." + provider + "." + name + ".log")
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stdout, cmd.Stderr = log, log
			err = cmd.Run()
			_ = log.Close()
			exited := time.Now()
			server.Close()
			samples, issues := r.result()
			row := report{Provider: provider, Case: name, BaselineDone: r.baselineDone, EndDone: r.endDone, Exited: exited, Issues: issues, Traces: r.traces, Samples: samples, Expected: expected}
			row.BackgroundAliveAtStop = r.backgroundAliveAtStop
			if err != nil {
				row.ProcessError = err.Error()
			}
			if r.turn != nil {
				row.Start, row.End = r.turn.StartTime, r.turn.EndTime
			}
			reports = append(reports, row)
			data, _ := json.MarshalIndent(reports, "", "  ")
			if err := os.WriteFile(output, data, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Logf("provider=%s case=%s start=%s end=%s issues=%v process=%s", provider, name, row.Start, row.End, issues, row.ProcessError)
		})
	}
}

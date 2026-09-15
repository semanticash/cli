//go:build turnexperiment

package toolsnap_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/hooks/claude"
	"github.com/semanticash/cli/internal/hooks/codex"
)

// TestTurnExperimentHook runs in a child process invoked by a temporary hook.
// Parsing uses the installed adapters; Dispatch and persistence are not called.
func TestTurnExperimentHook(t *testing.T) {
	endpoint := os.Getenv("TURN_HOOK_ENDPOINT")
	if endpoint == "" {
		t.Skip("hook subprocess only")
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 4<<20))
	if err != nil {
		t.Fatal(err)
	}
	var input struct {
		Event string `json:"hook_event_name"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		t.Fatal(err)
	}
	name := map[string]string{"UserPromptSubmit": "user-prompt-submit", "Stop": "stop"}[input.Event]
	var event *hooks.Event
	if name != "" {
		var provider hooks.HookProvider = codex.New()
		if os.Getenv("TURN_HOOK_PROVIDER") == "claude" {
			provider = claude.New()
		}
		event, err = provider.ParseHookEvent(context.Background(), name, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
	}
	message, err := json.Marshal(struct {
		Raw   json.RawMessage `json:"raw"`
		Event *hooks.Event    `json:"event,omitempty"`
	}{raw, event})
	if err != nil {
		t.Fatal(err)
	}
	client := http.Client{Timeout: 90 * time.Second}
	response, err := client.Post(endpoint, "application/json", bytes.NewReader(message))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("hook receiver: %s", response.Status)
	}
}

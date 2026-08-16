package commands

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/providers"
	"github.com/semanticash/cli/internal/store/blobs"
	"github.com/semanticash/cli/internal/util"
)

// stdinReadDeadline bounds how long capture waits for a complete JSON payload.
const stdinReadDeadline = 250 * time.Millisecond

// processStart approximates process startup for hook timing.
var processStart = time.Now()

// NewCaptureCmd creates the `semantica capture <provider> <event-name>` command.
// This command is used by provider hook configurations and is not intended
// for interactive use.
func NewCaptureCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "capture <provider> <event-name>",
		Short:  "Internal hook capture entrypoint",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		// Always exit 0 so provider hooks remain non-fatal.
		// Errors are logged, not returned.
		RunE: func(cmd *cobra.Command, args []string) error {
			providerName := args[0]
			hookName := args[1]
			ctx := hooks.WithHookStart(cmd.Context(), processStart)

			// Broker-wide enabled check: any active repo in the registry?
			// Do not gate on the cwd repo's local .semantica/ settings:
			// a session from repo A may be editing repo B.
			registryPath, err := broker.DefaultRegistryPath()
			if err != nil {
				return nil
			}
			bh, err := broker.Open(ctx, registryPath)
			if err != nil {
				return nil
			}
			defer func() { _ = broker.Close(bh) }()

			repos, err := broker.ListActiveRepos(ctx, bh)
			if err != nil || len(repos) == 0 {
				// No active repos means hooks are effectively dormant.
				return nil
			}

			// Look up the provider from the production registry. The
			// registry is cheap to construct and holds stateless values.
			provider := providers.NewHookRegistry().Get(providerName)
			if provider == nil {
				logCaptureError(providerName, hookName, "unknown provider: %s", providerName)
				return nil
			}

			// Read stdin once so providers implementing CwdGatedProvider
			// can inspect the payload before any side effects, and so we
			// can replay the same bytes into ParseHookEvent afterward.
			// Bound the read so a hook runner that leaves stdin open
			// cannot block capture indefinitely. Providers that need a
			// payload will fail gracefully when the deadline elapses.
			payload, timedOut, err := readHookPayload(os.Stdin, stdinReadDeadline)
			if err != nil {
				logCaptureError(providerName, hookName, "read stdin (%s/%s): %v", providerName, hookName, err)
				return nil
			}
			if timedOut {
				logCaptureError(providerName, hookName, "complete hook payload not received before %s; continuing with empty hook payload", stdinReadDeadline)
			}

			// Optional cwd preflight. Used by providers whose hook
			// configuration is user-global (and therefore fires on every
			// session on the machine) to suppress side effects when the
			// session originates from a repo that is not registered with
			// Semantica. Suppression is silent: no parsing, no blob
			// store, no broker writes, no hook-error entry.
			if gated, ok := provider.(hooks.CwdGatedProvider); ok {
				allow, gerr := gated.ShouldCapture(ctx, payload, repos)
				if gerr != nil || !allow {
					return nil
				}
			}

			// Parse event from the buffered payload.
			event, err := provider.ParseHookEvent(ctx, hookName, bytes.NewReader(payload))
			if err != nil {
				logCaptureError(providerName, hookName, "parse hook event (%s/%s): %v", providerName, hookName, err)
				return nil
			}
			if event == nil {
				// Provider returned nil because this hook does not produce a capture event.
				return nil
			}

			// Open global blob store for payload capture.
			// Blobs are stored here at hook time, then copied into per-repo
			// stores by WriteEventsToRepo during routing.
			var blobStore *blobs.Store
			if objDir, err := broker.GlobalObjectsDir(); err == nil {
				if bs, err := blobs.NewStore(objDir); err != nil {
					logCaptureError(providerName, hookName, "global blob store: %v (attribution will degrade)", err)
				} else {
					blobStore = bs
				}
			}

			// Dispatch lifecycle event.
			if err := hooks.Dispatch(ctx, provider, event, bh, blobStore); err != nil {
				logCaptureError(providerName, hookName, "dispatch (%s/%s): %v", providerName, hookName, err)
			}

			return nil
		},
	}

	return cmd
}

// readHookPayload decodes one JSON value without waiting for EOF. It reports a
// deadline separately from read and decode errors.
// A timed-out decoder may remain blocked until the hook process exits.
func readHookPayload(r io.Reader, deadline time.Duration) (payload []byte, timedOut bool, err error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		var data json.RawMessage
		e := json.NewDecoder(r).Decode(&data)
		if e == io.EOF {
			e = nil
		}
		ch <- result{data: data, err: e}
	}()
	select {
	case res := <-ch:
		return res.data, false, res.err
	case <-time.After(deadline):
		return nil, true, nil
	}
}

// logCaptureError writes a capture failure to stderr and doctor diagnostics.
func logCaptureError(provider, hook, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "semantica capture: %s\n", msg)
	util.AppendHookError(provider, hook, msg)
}

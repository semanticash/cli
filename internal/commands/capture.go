package commands

import (
	"bytes"
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

// stdinReadDeadline bounds how long capture waits for EOF on stdin.
// Hook runners normally pipe a JSON payload and close stdin promptly.
// Some hosts inherit an open stdin instead, so an unbounded io.ReadAll
// can block until the hook runner times out.
const stdinReadDeadline = 250 * time.Millisecond

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
			ctx := cmd.Context()

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
				logCaptureError(providerName, hookName, "stdin did not close before %s; continuing with empty hook payload", stdinReadDeadline)
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

// readHookPayload is the bounded equivalent of io.ReadAll for hook
// payloads.
//
// Returns one of three outcomes:
//
//   - (payload, false, nil): the reader closed before the deadline.
//   - (nil,     true,  nil): the deadline elapsed first.
//   - (nil,     false, err): the read failed.
//
// On timeout, the read goroutine may remain blocked until process exit.
// Capture is short-lived, so the caller prefers that to blocking the hook.
func readHookPayload(r io.Reader, deadline time.Duration) (payload []byte, timedOut bool, err error) {
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		data, e := io.ReadAll(r)
		ch <- result{data: data, err: e}
	}()
	select {
	case res := <-ch:
		return res.data, false, res.err
	case <-time.After(deadline):
		return nil, true, nil
	}
}

// logCaptureError reports a capture-time failure on stderr (for
// developers running interactively) and appends a structured entry
// to the global hook-errors sidecar log so `semantica doctor` can
// surface it later. The shell `|| true` wrapper around hook
// invocations still swallows our exit code, which is the contract
// keeping hooks non-blocking.
func logCaptureError(provider, hook, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintf(os.Stderr, "semantica capture: %s\n", msg)
	util.AppendHookError(provider, hook, msg)
}

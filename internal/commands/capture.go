package commands

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/semanticash/cli/internal/broker"
	"github.com/semanticash/cli/internal/hooks"
	"github.com/semanticash/cli/internal/store/blobs"
	"github.com/semanticash/cli/internal/util"

	// Register hook providers via init().
	_ "github.com/semanticash/cli/internal/hooks/claude"
	_ "github.com/semanticash/cli/internal/hooks/copilot"
	_ "github.com/semanticash/cli/internal/hooks/cursor"
	_ "github.com/semanticash/cli/internal/hooks/gemini"
	_ "github.com/semanticash/cli/internal/hooks/kirocli"
	_ "github.com/semanticash/cli/internal/hooks/kiroide"
)

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

			// Broker-global enabled check: any active repo in the registry?
			// Do NOT gate on the cwd repo's local .semantica/ settings -
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
				// No active repos - hooks are effectively dormant.
				return nil
			}

			// Look up provider.
			provider := hooks.GetProvider(providerName)
			if provider == nil {
				logCaptureError(providerName, hookName, "unknown provider: %s", providerName)
				return nil
			}

			// Parse event from stdin.
			event, err := provider.ParseHookEvent(ctx, hookName, os.Stdin)
			if err != nil {
				logCaptureError(providerName, hookName, "parse hook event (%s/%s): %v", providerName, hookName, err)
				return nil
			}
			if event == nil {
				// Provider returned nil - this hook does not produce a capture event.
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

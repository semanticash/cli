package commands

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/version"
	"github.com/spf13/cobra"
)

type RootOptions struct {
	RepoPath string // optional override (default: cwd)
}

func NewRootCmd() *cobra.Command {
	opts := &RootOptions{}

	cmd := &cobra.Command{
		Use:     "semantica",
		Short:   "Code, with provenance.",
		Version: version.Display(),
	}
	cmd.SetVersionTemplate("{{.Version}}\n")

	cmd.PersistentFlags().StringVar(&opts.RepoPath, "repo", "", "path to git repository (default: current directory)")

	// Subcommands
	cmd.AddCommand(NewEnableCmd(opts))
	cmd.AddCommand(NewDisableCmd(opts))
	cmd.AddCommand(NewCheckpointCmd(opts))
	cmd.AddCommand(NewListCmd(opts))
	cmd.AddCommand(NewRewindCmd(opts))
	cmd.AddCommand(NewHookCmd(opts))
	cmd.AddCommand(NewShowCmd(opts))
	cmd.AddCommand(NewWorkerCmd(opts))
	cmd.AddCommand(NewTranscriptsCmd(opts))
	cmd.AddCommand(NewBlameCmd(opts))
	cmd.AddCommand(NewSessionsCmd(opts))
	cmd.AddCommand(NewExplainCmd(opts))
	cmd.AddCommand(NewStatusCmd(opts))
	cmd.AddCommand(NewSetCmd(opts))
	cmd.AddCommand(NewAgentsCmd(opts))
	cmd.AddCommand(NewSuggestCmd(opts))
	cmd.AddCommand(NewTidyCmd(opts))
	cmd.AddCommand(NewAuthCmd())
	cmd.AddCommand(NewConnectCmd(opts))
	cmd.AddCommand(NewDisconnectCmd(opts))
	cmd.AddCommand(NewWorkspaceCmd())
	cmd.AddCommand(NewGeneratePlaybookCmd())
	cmd.AddCommand(NewAutoPlaybookCmd())
	cmd.AddCommand(NewAutoImplementationSummaryCmd())
	cmd.AddCommand(NewCaptureCmd())
	cmd.AddCommand(NewImplementationsCmd(opts))
	cmd.AddCommand(newVersionCmd())

	return cmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), version.Display())
		},
	}
}

func Execute() {
	// Create context that cancels on interrupt/termination.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle interrupts
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, platform.TermSignals()...)
	go func() {
		<-sigChan
		cancel()
	}()

	rootCommand := NewRootCmd()
	err := rootCommand.ExecuteContext(ctx)
	if err != nil {
		out := rootCommand.OutOrStderr()

		msg := err.Error()
		switch {
		case strings.Contains(msg, "unknown command") || strings.Contains(msg, "unknown flag"):
			_, _ = fmt.Fprintln(out, msg)
			_, _ = fmt.Fprintln(out)
			_ = rootCommand.Usage()
		default:
			_, _ = fmt.Fprintln(out, msg)
		}

		// Ensure cancellation cascades
		cancel()
		os.Exit(1)
	}

}

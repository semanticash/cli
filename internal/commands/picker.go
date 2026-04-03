package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/semanticash/cli/internal/service"
	"github.com/semanticash/cli/internal/util"
)

// resolveRef returns a ref from args, or shows an interactive checkpoint
// picker if no arg is given and stdin is a TTY.
func resolveRef(ctx context.Context, repoPath string, args []string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}

	if !isTerminal() {
		return "", fmt.Errorf("missing argument: <ref>")
	}

	return pickCheckpoint(ctx, repoPath)
}

// pickCheckpoint shows an interactive select list of recent checkpoints.
func pickCheckpoint(ctx context.Context, repoPath string) (string, error) {
	svc := service.NewListService()
	res, err := svc.ListCheckpoints(ctx, service.ListCheckpointsInput{
		RepoPath: repoPath,
		Limit:    20,
	})
	if err != nil {
		return "", err
	}

	if len(res.Items) == 0 {
		return "", fmt.Errorf("no checkpoints found")
	}

	options := make([]huh.Option[string], len(res.Items))
	for i, it := range res.Items {
		label := formatCheckpointOption(it)
		options[i] = huh.NewOption(label, it.ID)
	}

	var selected string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewSelect[string]().
				Title("Select a checkpoint").
				Options(options...).
				Value(&selected),
		),
	)

	if err := form.Run(); err != nil {
		return "", fmt.Errorf("missing argument: <ref>")
	}

	if selected == "" {
		return "", fmt.Errorf("no checkpoint selected")
	}

	return selected, nil
}

// formatCheckpointOption builds a compact one-line label for a checkpoint.
func formatCheckpointOption(it service.ListedCheckpoint) string {
	id := util.ShortID(it.ID)
	age := relativeAge(it.CreatedAt)

	var parts []string
	parts = append(parts, id)
	parts = append(parts, age)
	parts = append(parts, it.Kind)

	if it.CommitHash != "" {
		parts = append(parts, util.ShortID(it.CommitHash))
	}

	subject := strings.TrimSpace(it.CommitSubject)
	if subject != "" {
		if len(subject) > 50 {
			subject = subject[:47] + "..."
		}
		parts = append(parts, subject)
	}

	return strings.Join(parts, "  ")
}

func relativeAge(ms int64) string {
	d := time.Since(time.UnixMilli(ms))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

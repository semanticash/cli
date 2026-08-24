package health

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/service"
)

// checkCommitReceipts reports invalid receipts that block commit processing.
func checkCommitReceipts(opts Options) []Check {
	if opts.RepoPath == "" {
		return nil
	}
	semDir := filepath.Join(opts.RepoPath, ".semantica")
	problems := service.InspectCommitReceipts(semDir)
	if len(problems) == 0 {
		return nil
	}
	lines := make([]string, len(problems))
	for i, p := range problems {
		lines[i] = fmt.Sprintf("%s (%s)", p.File, p.Reason)
	}
	return []Check{{
		Category: "worker", ID: "commit_receipts", Status: StatusFail,
		Message: fmt.Sprintf("%d invalid commit receipt(s) blocking commit sequencing: %s",
			len(problems), strings.Join(lines, "; ")),
		Remediation: "inspect .semantica/commit-receipts/, repair or remove the listed file(s), then run `semantica worker drain`",
	}}
}

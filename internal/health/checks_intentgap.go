package health

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/util"
)

// activityLogTailLines bounds local log scanning for doctor.
const activityLogTailLines = 500

// checkIntentGap reports local configuration and recent activity without
// requiring network access.
func checkIntentGap(opts Options) []Check {
	var checks []Check

	if opts.RepoPath == "" {
		// Non-repository contexts do not need intent-gap diagnostics.
		return checks
	}

	semDir := filepath.Join(opts.RepoPath, ".semantica")
	if _, err := os.Stat(semDir); err != nil {
		// Repositories without Semantica state should not produce warnings.
		return checks
	}

	if util.IntentGapEnabled(semDir) {
		checks = append(checks, Check{
			Category: "intent-gap",
			ID:       "setting",
			Status:   StatusOK,
			Message:  "manual intent-gap analysis enabled",
		})
	} else {
		checks = append(checks, Check{
			Category: "intent-gap",
			ID:       "setting",
			Status:   StatusOK,
			Message:  "manual intent-gap analysis disabled (off by default)",
		})
	}

	checks = append(checks, lastIntentGapActivity(semDir))
	return checks
}

// lastIntentGapActivity reports the latest relevant local activity entry.
func lastIntentGapActivity(semDir string) Check {
	path := filepath.Join(semDir, "activity.log")
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return Check{
			Category: "intent-gap",
			ID:       "last_activity",
			Status:   StatusOK,
			Message:  "no recorded intent-gap activity yet",
		}
	case err != nil:
		return Check{
			Category:    "intent-gap",
			ID:          "last_activity",
			Status:      StatusWarn,
			Message:     "activity.log unreadable: " + err.Error(),
			Remediation: "check filesystem permissions on .semantica/activity.log",
		}
	}

	line, ok := mostRecentIntentGapLine(string(data))
	if !ok {
		return Check{
			Category: "intent-gap",
			ID:       "last_activity",
			Status:   StatusOK,
			Message:  "no recorded intent-gap activity yet",
		}
	}

	status := StatusOK
	remediation := ""
	if isUploadFailureLine(line) {
		status = StatusWarn
		remediation = "run `semantica intent-gap analyze` to retry"
	}
	return Check{
		Category:    "intent-gap",
		ID:          "last_activity",
		Status:      status,
		Message:     "last activity: " + line,
		Remediation: remediation,
	}
}

// isUploadFailureLine recognizes analysis and upload failures.
func isUploadFailureLine(line string) bool {
	switch {
	case strings.Contains(line, "intent-gap upload error"):
		return true
	case strings.Contains(line, "intent-gap error:"):
		return true
	case strings.Contains(line, "intent-gap analysis errored"):
		// Recording an errored row does not make the analysis successful.
		return true
	}
	return false
}

// mostRecentIntentGapLine returns the latest relevant non-empty log line.
func mostRecentIntentGapLine(logBody string) (string, bool) {
	lines := strings.Split(logBody, "\n")
	start := len(lines) - activityLogTailLines
	if start < 0 {
		start = 0
	}
	for i := len(lines) - 1; i >= start; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if !strings.Contains(line, "intent-gap") {
			continue
		}
		return line, true
	}
	return "", false
}

package health

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/semanticash/cli/internal/util"
)

// activityLogTailLines bounds how far back the intent-gap check reads
// the activity log when looking for the most recent intent-gap or
// pre-push line. The activity log is shared with other capture-side
// events, so older entries can be plentiful; capping the tail keeps
// doctor fast on long-lived repos.
const activityLogTailLines = 500

// checkIntentGap surfaces the intent-gap setting state and the most
// recent intent-gap or pre-push activity line in the repository. The
// checks are purely local: no HTTP, no git command, so they stay
// fast and offline-friendly. Network-dependent diagnostics (e.g.
// "open PR at HEAD but no upload yet") belong on `intent-gap analyze`
// instead, which already produces a skip result with a clear reason.
func checkIntentGap(opts Options) []Check {
	var checks []Check

	if opts.RepoPath == "" {
		// Non-repo context: skip without noise so the rest of the
		// report stays readable.
		return checks
	}

	semDir := filepath.Join(opts.RepoPath, ".semantica")
	if _, err := os.Stat(semDir); err != nil {
		// Keep doctor quiet for repos without local Semantica state.
		return checks
	}

	if util.IntentGapEnabled(semDir) {
		checks = append(checks, Check{
			Category: "intent-gap",
			ID:       "setting",
			Status:   StatusOK,
			Message:  "intent-gap uploads enabled",
		})
	} else {
		checks = append(checks, Check{
			Category: "intent-gap",
			ID:       "setting",
			Status:   StatusOK,
			Message:  "intent-gap uploads disabled (off by default)",
		})
	}

	checks = append(checks, lastIntentGapActivity(semDir))
	return checks
}

// lastIntentGapActivity tails the activity log and returns the most
// recent line whose body mentions intent-gap or the pre-push trigger.
// The result is informational; an upload error line surfaces as a
// warning with a remediation pointing at `semantica intent-gap
// analyze`. Absence of any line is a normal-state OK.
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

// isUploadFailureLine reports whether an activity line records a
// failed intent-gap upload or a pre-push failure that prevented the
// background worker from starting.
func isUploadFailureLine(line string) bool {
	switch {
	case strings.Contains(line, "intent-gap upload error"):
		return true
	case strings.Contains(line, "intent-gap error:"):
		return true
	case strings.Contains(line, "pre-push warning:"):
		return true
	case strings.Contains(line, "pre-push:") && strings.Contains(line, "failed"):
		return true
	}
	return false
}

// mostRecentIntentGapLine returns the most recent non-empty activity
// log line that mentions either the intent-gap upload path or the
// pre-push trigger that drives it. The boolean reports whether any
// such line was found.
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
		if !strings.Contains(line, "intent-gap") && !strings.Contains(line, "pre-push") {
			continue
		}
		return line, true
	}
	return "", false
}

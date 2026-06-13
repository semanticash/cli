package health

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/util"
)

// hookErrorWindow is the lookback used when summarising the
// hook-errors.log sidecar.
const hookErrorWindow = 24 * time.Hour

// hookErrorTailLines bounds how far back doctor reads the sidecar.
// 200 lines is plenty for any 24h window with a few real failures
// and keeps the worst-case parse work bounded.
const hookErrorTailLines = 200

// claudeTrackedSettingsBasename is the tracked Claude settings
// file. When Semantica hooks land here they get committed and
// teammates without Semantica see hook errors.
const claudeTrackedSettingsBasename = "settings.json"

// claudeMarker is the substring that identifies a Semantica-owned
// Claude Code hook. Mirrors the constant defined in
// internal/hooks/claude/claude.go.
const claudeMarker = "semantica capture claude-code"

// checkHookErrors reads the global hook-errors.log sidecar and
// reports recent failures grouped by `provider/hook`. The sidecar
// is populated by `logCaptureError` in
// internal/commands/capture.go; hooks remain non-blocking.
func checkHookErrors(_ context.Context) []Check {
	entries, err := util.ReadHookErrorTail(hookErrorTailLines)
	if err != nil {
		// ReadHookErrorTail returns (nil, nil) when the file is
		// absent. A non-nil error means the file exists but doctor
		// cannot inspect it, so report the diagnostic gap.
		path := "<unknown>"
		if p, perr := util.HookErrorLogPath(); perr == nil {
			path = p
		}
		return []Check{{
			Category:    "diagnostics",
			ID:          "hook_errors",
			Status:      StatusWarn,
			Message:     "hook-errors.log unreadable: " + err.Error(),
			Remediation: "check filesystem permissions on " + path,
		}}
	}
	if len(entries) == 0 {
		return []Check{{
			Category: "diagnostics",
			ID:       "hook_errors",
			Status:   StatusOK,
			Message:  "no hook errors recorded",
		}}
	}

	since := time.Now().UTC().Add(-hookErrorWindow)
	var recent []util.HookErrorEntry
	for _, e := range entries {
		ts, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil {
			continue
		}
		if ts.Before(since) {
			continue
		}
		recent = append(recent, e)
	}
	if len(recent) == 0 {
		return []Check{{
			Category: "diagnostics",
			ID:       "hook_errors",
			Status:   StatusOK,
			Message:  "no hook errors in the last 24h",
		}}
	}

	groups := groupHookErrors(recent)
	var checks []Check

	checks = append(checks, Check{
		Category:    "diagnostics",
		ID:          "hook_errors",
		Status:      StatusWarn,
		Message:     fmt.Sprintf("%d hook error(s) in the last 24h across %d provider/hook pair(s)", len(recent), len(groups)),
		Remediation: hookErrorLogRemediation(),
	})

	const showTop = 3
	for i, g := range groups {
		if i >= showTop {
			break
		}
		checks = append(checks, Check{
			Category: "diagnostics",
			ID:       fmt.Sprintf("hook_errors:%d", i+1),
			Status:   StatusWarn,
			Message:  fmt.Sprintf("%s: %d", g.label, g.count),
		})
	}
	return checks
}

type hookErrorGroup struct {
	label string
	count int
}

func groupHookErrors(entries []util.HookErrorEntry) []hookErrorGroup {
	counts := map[string]int{}
	for _, e := range entries {
		label := hookErrorLabel(e)
		counts[label]++
	}
	groups := make([]hookErrorGroup, 0, len(counts))
	for k, v := range counts {
		groups = append(groups, hookErrorGroup{label: k, count: v})
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].count != groups[j].count {
			return groups[i].count > groups[j].count
		}
		return groups[i].label < groups[j].label
	})
	return groups
}

// hookErrorLabel renders a stable "provider/hook" label, falling
// back to the first short message segment when provider/hook are
// missing.
func hookErrorLabel(e util.HookErrorEntry) string {
	switch {
	case e.Provider != "" && e.Hook != "":
		return e.Provider + "/" + e.Hook
	case e.Provider != "":
		return e.Provider
	}
	msg := e.Message
	if i := strings.Index(msg, ":"); i > 0 {
		return strings.TrimSpace(msg[:i])
	}
	if len(msg) > 60 {
		msg = msg[:60] + "..."
	}
	return msg
}

func hookErrorLogRemediation() string {
	if path, err := util.HookErrorLogPath(); err == nil {
		return "inspect " + path + " for full error context"
	}
	return "inspect hook-errors.log under your Semantica config dir"
}

// checkProviderFootguns flags provider-specific configurations that
// can cause confusing or missing capture behavior.
//
//   - Kiro IDE: hooks require per-command Trusted Commands approval;
//     doctor cannot probe the trust list from disk, so it surfaces a
//     soft hint whenever Kiro IDE hooks are installed (informational,
//     not a warn).
//
//   - Claude Code: Semantica hooks should live in `settings.local.json`
//     (gitignored). When they leak into the tracked `settings.json`
//     they get committed, breaking teammates without Semantica.
func checkProviderFootguns(ctx context.Context, opts Options) []Check {
	if opts.RepoPath == "" {
		return nil
	}

	var checks []Check
	for _, p := range listRegistryProviders(opts) {
		if !p.AreHooksInstalled(ctx, opts.RepoPath) {
			continue
		}
		switch p.Name() {
		case "kiro-ide":
			checks = append(checks, Check{
				Category:    "footguns",
				ID:          "kiro_trust_gate",
				Status:      StatusOK,
				Message:     "Kiro IDE 0.11+ requires Trusted Commands approval; doctor cannot verify from disk",
				Remediation: "in Kiro Settings > Trusted Commands, ensure `semantica capture kiro-ide ...` is approved",
			})
		case "claude-code":
			if c, ok := claudeTrackedHookCheck(opts.RepoPath); ok {
				checks = append(checks, c)
			}
		}
	}
	return checks
}

// claudeTrackedHookCheck warns when Semantica's Claude hooks are in
// a `.claude/settings.json` that git will actually commit. Many repos
// keep both `settings.json` and `settings.local.json` ignored locally;
// the warning is only shown after confirming git does not ignore the
// file.
//
// Returns ok=false when the file is missing, has no Semantica marker,
// or git says the path is ignored. If git cannot confirm the path is
// ignored, the warning is shown.
func claudeTrackedHookCheck(repoPath string) (Check, bool) {
	tracked := filepath.Join(repoPath, ".claude", claudeTrackedSettingsBasename)
	data, err := os.ReadFile(tracked)
	if err != nil {
		return Check{}, false
	}
	if !strings.Contains(string(data), claudeMarker) {
		return Check{}, false
	}
	if isGitIgnored(repoPath, tracked) {
		return Check{}, false
	}
	return Check{
		Category:    "footguns",
		ID:          "claude_tracked_settings",
		Status:      StatusWarn,
		Message:     "Claude Code hooks are configured in .claude/settings.json, which is not gitignored",
		Remediation: "move Semantica hooks to .claude/settings.local.json (gitignored) so teammates without Semantica are unaffected",
	}, true
}

// isGitIgnored reports whether `git check-ignore` considers path
// ignored relative to repoPath. Any non-zero result is treated as
// "not confirmed ignored".
func isGitIgnored(repoPath, path string) bool {
	cmd := exec.Command("git", "-C", repoPath, "check-ignore", "--quiet", path)
	err := cmd.Run()
	if err == nil {
		// Exit 0: path is ignored.
		return true
	}
	// Exit 1 means not ignored; 128 and other non-zero statuses mean git
	// could not answer. In both cases the caller should keep the warning.
	return false
}

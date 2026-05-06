package health

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqldb "github.com/semanticash/cli/internal/store/sqlite/db"
)

// reasonGroupKey is the contract that makes doctor's failed-manifest
// grouping deterministic. The "redaction failed: <kind>" prefix
// produced by `redactionFailedReason` in internal/provenance/sync.go
// is preserved here so redaction failures are distinguishable from
// missing-blob errors.

func TestReasonGroupKey_RedactionPrefixPreserved(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{
			raw:  "redaction failed: prompt: redact prompt: detector init failure",
			want: "redaction failed: prompt",
		},
		{
			raw:  "redaction failed: bundle: unmarshal: invalid character",
			want: "redaction failed: bundle",
		},
		{
			raw:  "redaction failed: step_provenance: tool_input: tool_fields object: foo",
			want: "redaction failed: step_provenance",
		},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := reasonGroupKey(tc.raw); got != tc.want {
				t.Errorf("reasonGroupKey(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestReasonGroupKey_MissingLocalBlob(t *testing.T) {
	// Hash content varies per failure, but the group should fold them all.
	a := "prompt blob abc12345 referenced by bundle but missing locally: read: not found"
	b := "step_provenance blob deadbeef referenced by bundle but missing locally: read: not found"
	if got := reasonGroupKey(a); got != "missing local blob" {
		t.Errorf("reasonGroupKey(prompt) = %q, want missing local blob", got)
	}
	if got := reasonGroupKey(b); got != "missing local blob" {
		t.Errorf("reasonGroupKey(step) = %q, want missing local blob", got)
	}
}

func TestReasonGroupKey_FirstSegmentFallback(t *testing.T) {
	got := reasonGroupKey("marshal envelope: write: pipe broken")
	if got != "marshal envelope" {
		t.Errorf("reasonGroupKey marshal envelope: got %q", got)
	}
}

func TestReasonGroupKey_NoColonReturnsFull(t *testing.T) {
	got := reasonGroupKey("no bundle hash on manifest")
	if got != "no bundle hash on manifest" {
		t.Errorf("got %q", got)
	}
}

func TestGroupFailedReasons_FoldsAndSortsDescending(t *testing.T) {
	rows := []sqldb.ListFailedManifestReasonsRow{
		{LastError: "redaction failed: prompt: A", Count: 2},
		{LastError: "redaction failed: prompt: B", Count: 3},
		{LastError: "redaction failed: bundle: A", Count: 1},
		{LastError: "marshal envelope: write: pipe broken", Count: 1},
	}
	groups := groupFailedReasons(rows)
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups, got %d: %+v", len(groups), groups)
	}
	if groups[0].group != "redaction failed: prompt" || groups[0].count != 5 {
		t.Errorf("top group = %+v, want redaction failed: prompt / 5", groups[0])
	}
	// Ties broken alphabetically: "marshal envelope" < "redaction failed: bundle"
	if groups[1].count != 1 || groups[2].count != 1 {
		t.Errorf("expected count-1 tail, got %+v", groups[1:])
	}
}

func TestAssembleManifestChecks_NoFailures_OK(t *testing.T) {
	checks := assembleManifestChecks(map[string]int64{
		"uploaded": 10,
		"pending":  1,
	}, nil)
	if len(checks) != 1 {
		t.Fatalf("expected 1 check on healthy manifests, got %d", len(checks))
	}
	if checks[0].Status != StatusOK {
		t.Errorf("status = %q, want ok", checks[0].Status)
	}
	if !strings.Contains(checks[0].Message, "0 failed") {
		t.Errorf("expected 0 failed in message, got %q", checks[0].Message)
	}
}

func TestAssembleManifestChecks_FailedSurfacedAsWarn(t *testing.T) {
	rows := []sqldb.ListFailedManifestReasonsRow{
		{LastError: "redaction failed: prompt: detector init", Count: 5},
		{LastError: "missing bundle blob: not found", Count: 2},
	}
	checks := assembleManifestChecks(map[string]int64{
		"uploaded": 100,
		"failed":   7,
	}, rows)

	if checks[0].Status != StatusWarn {
		t.Errorf("summary status = %q, want warn", checks[0].Status)
	}
	if !strings.Contains(checks[0].Message, "7 failed") {
		t.Errorf("expected 7 failed in summary, got %q", checks[0].Message)
	}

	// Top reason gets a dedicated check with the redaction prefix and count.
	var sawRedactionRow bool
	for _, c := range checks {
		if c.ID == "failed_reason:1" {
			sawRedactionRow = true
			if !strings.Contains(c.Message, "redaction failed: prompt") {
				t.Errorf("top reason message missing prefix: %q", c.Message)
			}
			if !strings.Contains(c.Message, "(5)") {
				t.Errorf("top reason count missing: %q", c.Message)
			}
			if c.Remediation == "" {
				t.Error("redaction-prefix group should carry a remediation hint")
			}
		}
	}
	if !sawRedactionRow {
		t.Error("missing failed_reason:1 row")
	}
}

func TestAssembleManifestChecks_TruncatesPastShowLimit(t *testing.T) {
	rows := []sqldb.ListFailedManifestReasonsRow{
		{LastError: "a: 1", Count: 5},
		{LastError: "b: 2", Count: 4},
		{LastError: "c: 3", Count: 3},
		{LastError: "d: 4", Count: 2},
		{LastError: "e: 5", Count: 1},
	}
	checks := assembleManifestChecks(map[string]int64{"failed": 15}, rows)

	var sawOverflow bool
	for _, c := range checks {
		if c.ID == "failed_reason:other" {
			sawOverflow = true
			// 5 + 4 + 3 = 12 shown, 2 + 1 = 3 in overflow.
			if !strings.Contains(c.Message, "3 more") {
				t.Errorf("overflow count = %q, want '3 more'", c.Message)
			}
		}
	}
	if !sawOverflow {
		t.Error("missing failed_reason:other overflow row when there are more than failedManifestReasonsToShow groups")
	}
}

func TestStorageProviderCandidates_KnownTranslations(t *testing.T) {
	cases := []struct {
		hook string
		want []string
	}{
		{"claude-code", []string{"claude_code", "claude-code"}},
		{"gemini", []string{"gemini_cli", "gemini"}},
		{"cursor", []string{"cursor"}},
		{"kiro-ide", []string{"kiro-ide", "kiro_ide"}},
	}
	for _, tc := range cases {
		t.Run(tc.hook, func(t *testing.T) {
			got := storageProviderCandidates(tc.hook)
			if !equalSlices(got, tc.want) {
				t.Errorf("storageProviderCandidates(%q) = %v, want %v", tc.hook, got, tc.want)
			}
		})
	}
}

func TestHasEventsForHookProvider_FindsAcrossNameForms(t *testing.T) {
	events := map[string]int64{
		"claude_code": 47, // stored form
	}
	if !hasEventsForHookProvider("claude-code", events) {
		t.Error("expected claude-code (hook) to find claude_code events (storage)")
	}
}

func TestHasEventsForHookProvider_ReturnsFalseWhenAbsent(t *testing.T) {
	if hasEventsForHookProvider("cursor", map[string]int64{"copilot": 5}) {
		t.Error("expected no events for cursor when only copilot has them")
	}
}

func TestSummariseEvents_FormatsCountsAndAge(t *testing.T) {
	now := time.Now()
	events := map[string]int64{
		"claude_code": 47,
		"cursor":      3,
	}
	mostRecent := map[string]int64{
		"claude_code": now.Add(-8 * time.Minute).UnixMilli(),
		"cursor":      now.Add(-2 * time.Hour).UnixMilli(),
	}
	got := summariseEvents(events, mostRecent)
	if got.Status != StatusOK {
		t.Errorf("status = %q, want ok", got.Status)
	}
	if !strings.Contains(got.Message, "claude_code: 47") {
		t.Errorf("message missing claude_code count: %q", got.Message)
	}
	if !strings.Contains(got.Message, "cursor: 3") {
		t.Errorf("message missing cursor count: %q", got.Message)
	}
}

func TestSummariseEvents_EmptyShowsIdleNote(t *testing.T) {
	got := summariseEvents(nil, nil)
	if got.Status != StatusOK {
		t.Errorf("status = %q, want ok (empty is normal)", got.Status)
	}
	if !strings.Contains(got.Message, "idle is normal") {
		t.Errorf("expected idle note in message, got %q", got.Message)
	}
}

func TestRelativeAge_Buckets(t *testing.T) {
	now := time.Now()
	cases := []struct {
		when time.Time
		want string
	}{
		{now.Add(-30 * time.Second), "<1m"},
		{now.Add(-2 * time.Minute), "2m"},
		{now.Add(-90 * time.Minute), "1h"},
		{now.Add(-25 * time.Hour), "1d"},
		{now.Add(-72 * time.Hour), "3d"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := relativeAge(now, tc.when); got != tc.want {
				t.Errorf("relativeAge = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSameRepo(t *testing.T) {
	cases := []struct {
		name      string
		root      string
		candidate string
		want      bool
	}{
		{"identical", "/repo", "/repo", true},
		{"subdir-of-root", "/repo", "/repo/sub", true},
		{"sibling", "/repo", "/other", false},
		{"parent-not-subdir", "/repo/sub", "/repo", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameRepo(tc.root, tc.candidate); got != tc.want {
				t.Errorf("sameRepo(%q, %q) = %v, want %v", tc.root, tc.candidate, got, tc.want)
			}
		})
	}
}

func TestActiveProvidersForRepo_FiltersByCwdAndWindow(t *testing.T) {
	repoA := t.TempDir()
	repoB := t.TempDir()
	t.Setenv("HOME", t.TempDir())

	// Capture-state directory lives under broker.GlobalBase(), which
	// honors HOME via the per-platform helper. Resolve once and seed.
	baseDir, err := captureBaseDirForTest(t)
	if err != nil {
		t.Skipf("could not resolve capture base dir: %v", err)
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UnixMilli()
	since := now - (recentEventWindow.Milliseconds())
	stale := now - (recentEventWindow.Milliseconds() + int64(time.Hour/time.Millisecond))

	writeState(t, baseDir, "fresh-repoA", "claude-code", repoA, now)
	writeState(t, baseDir, "stale-repoA", "cursor", repoA, stale)
	writeState(t, baseDir, "fresh-repoB", "copilot", repoB, now)
	writeState(t, baseDir, "no-cwd", "gemini", "", now)
		// A state file with Timestamp == 0 must not count as
		// fresh activity, even when its CWD is inside this repo.
	writeState(t, baseDir, "zero-ts-repoA", "kiro-cli", repoA, 0)

	got := activeProvidersForRepo(repoA, since)

	if !got["claude-code"] {
		t.Errorf("expected claude-code active for repoA: %+v", got)
	}
	if got["cursor"] {
		t.Errorf("stale state must not count: %+v", got)
	}
	if got["copilot"] {
		t.Errorf("other-repo state must not count: %+v", got)
	}
	if got["gemini"] {
		t.Errorf("missing-CWD state must not be misattributed: %+v", got)
	}
	if got["kiro-cli"] {
		t.Errorf("zero-timestamp state must not count as active: %+v", got)
	}
}

func captureBaseDirForTest(t *testing.T) (string, error) {
	t.Helper()
		// Match the layout used by hooks.captureDir(): broker.GlobalBase()
		// resolves to <HOME>/.semantica on darwin/linux, then capture/.
	home := os.Getenv("HOME")
	if home == "" {
		return "", os.ErrNotExist
	}
	return filepath.Join(home, ".semantica", "capture"), nil
}

func writeState(t *testing.T, baseDir, sessionID, provider, cwd string, ts int64) {
	t.Helper()
	body := `{"session_id":"` + sessionID + `","provider":"` + provider + `","transcript_ref":"x","transcript_offset":0,"timestamp":` +
		formatInt(ts) + `,"cwd":"` + cwd + `"}`
	path := filepath.Join(baseDir, "capture-"+sessionID+".json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

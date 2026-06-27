package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"

	"github.com/semanticash/cli/internal/service"
)

// NewIntentGapCmd creates the intent-gap command group.
func NewIntentGapCmd(rootOpts *RootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "intent-gap",
		Short: "Run and inspect intent-gap analysis",
	}

	cmd.AddCommand(newIntentGapAnalyzeCmd(rootOpts))

	return cmd
}

// newIntentGapAnalyzeCmd runs manual PR intent-gap analysis in the foreground.
func newIntentGapAnalyzeCmd(rootOpts *RootOptions) *cobra.Command {
	var base string
	var quiet bool
	var upload bool

	cmd := &cobra.Command{
		Use:   "analyze",
		Short: "Analyze the current PR with your installed AI agent",
		Long: `Resolves the open PR for the current branch, assembles a bundle
of commits and the cumulative diff, and runs intent-gap analysis using
your installed AI CLI fallback chain (Claude Code, Codex, Cursor,
Gemini CLI, GitHub Copilot CLI, or Kiro CLI).

By default the findings are printed to stdout and cached locally under
.semantica/intent-gap/ keyed on the current head SHA; the findings
themselves are NOT uploaded. PR discovery still queries the connected
workspace once to resolve the open PR for the current branch. Pass
--upload to record the findings server-side; if a cached analysis
already matches the current head SHA, requested base, prompt template
version, and finding schema version, --upload reuses it instead of
invoking the LLM again.

Skip conditions (exit 0, reason in output):
  - Semantica not enabled in this repo.
  - Repo not connected to a workspace.
  - No open PR for the current branch (or more than one).
  - No AI CLI installed.

Non-zero exit:
  - The analyzer ran but failed (LLM unavailable, parse error, schema
    error). With --upload an errored row is still recorded server-side
    so doctor and the dashboard see the failure.
  - With --upload, the wire upload itself failed (network / server error).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			svc := service.NewIntentGapUploadService(service.IntentGapUploadDeps{BaseRef: base})
			out := cmd.OutOrStdout()

			var res *service.IntentGapUploadResult
			var err error
			actionRan := false
			action := func() {
				actionRan = true
				res, err = svc.Run(cmd.Context(), rootOpts.RepoPath, service.RunOptions{Upload: upload})
			}
			spinnerTitle := "Analyzing pull request for intent gaps..."
			if upload {
				spinnerTitle = "Analyzing and uploading intent-gap findings..."
			}
			// Run the analysis at most once. If spinner setup fails before
			// the action starts, fall back to a direct foreground run.
			_ = runWithOptionalSpinner(out, quiet, spinnerTitle, action)
			if !actionRan {
				action()
			}
			if err != nil {
				return err
			}
			return renderAnalyzeResult(out, quiet, res)
		},
	}

	cmd.Flags().StringVar(&base, "base", "", "Base branch or ref (default: auto-detect)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "Suppress non-error output")
	cmd.Flags().BoolVar(&upload, "upload", false, "Record findings to the connected workspace (reuses the local cache when fresh)")

	return cmd
}

// renderAnalyzeResult renders the upload outcome. Analyzer and transport
// failures return errors even when an errored row was recorded successfully.
func renderAnalyzeResult(out io.Writer, quiet bool, res *service.IntentGapUploadResult) error {
	// Local-only run (no --upload): show findings and the cache hint,
	// then return. Errored analyses surface their reason here too -
	// nothing has been recorded server-side in this mode.
	if res.Status == service.UploadStatusAnalyzed {
		if res.Analysis == service.AnalysisErrored {
			if !quiet {
				_, _ = fmt.Fprintf(out, "Intent-gap analysis errored (%s) for PR #%d. Nothing uploaded.\n",
					res.AnalysisReason, res.PRNumber)
			}
			return fmt.Errorf("intent-gap analysis errored: %s", res.AnalysisReason)
		}
		if !quiet {
			renderFindingsLayout(out, res)
		}
		return nil
	}

	// Upload-mode paths from here on.
	if res.Analysis == service.AnalysisErrored && res.Status != service.UploadStatusError {
		if !quiet {
			_, _ = fmt.Fprintf(out, "Intent-gap analysis errored (%s); errored row recorded for PR #%d\n",
				res.AnalysisReason, res.PRNumber)
		}
		return fmt.Errorf("intent-gap analysis errored: %s", res.AnalysisReason)
	}

	switch res.Status {
	case service.UploadStatusUploaded:
		if !quiet {
			renderFindingsLayout(out, res)
			_, _ = fmt.Fprintf(out, "\nRecorded for PR #%d (upload_id=%s)", res.PRNumber, res.UploadID)
			if res.UsedCache {
				_, _ = fmt.Fprint(out, " (from local cache)")
			}
			_, _ = fmt.Fprintln(out)
		}
		return nil
	case service.UploadStatusDuplicate:
		if !quiet {
			renderFindingsLayout(out, res)
			_, _ = fmt.Fprintf(out, "\nAlready recorded for PR #%d (upload_id=%s)", res.PRNumber, res.UploadID)
			if res.UsedCache {
				_, _ = fmt.Fprint(out, " (from local cache)")
			}
			_, _ = fmt.Fprintln(out)
		}
		return nil
	case service.UploadStatusSkipped:
		if !quiet {
			_, _ = fmt.Fprintf(out, "Skipped: %s\n", res.Reason)
		}
		return nil
	case service.UploadStatusError:
		// Preserve a non-zero exit for scripts; details are in the activity log.
		return fmt.Errorf("intent-gap upload failed to record: %s", res.Reason)
	default:
		return fmt.Errorf("intent-gap analyze: unknown status %q", res.Status)
	}
}

// renderFindingsLayout prints findings grouped by kind followed by
// coverage. The cache file contains the same data as JSON.
func renderFindingsLayout(out io.Writer, res *service.IntentGapUploadResult) {
	_, _ = fmt.Fprintf(out, "Intent-gap analysis for PR #%d", res.PRNumber)
	if res.HeadSHA != "" {
		_, _ = fmt.Fprintf(out, " (head: %s", shortSHA(res.HeadSHA))
		if res.BaseSHA != "" {
			_, _ = fmt.Fprintf(out, ", base: %s", shortSHA(res.BaseSHA))
		}
		_, _ = fmt.Fprint(out, ")")
	}
	_, _ = fmt.Fprintln(out)
	if res.UsedCache {
		_, _ = fmt.Fprintln(out, "(reused cached analysis; no LLM call this run)")
	}

	groups := parseFindingsByKind(res.Findings)
	kinds := []string{"under_impl", "deferred", "unrequested"}
	emitted := 0
	for _, kind := range kinds {
		items := groups[kind]
		_, _ = fmt.Fprintf(out, "\n  %s (%d)\n", kind, len(items))
		if len(items) == 0 {
			continue
		}
		for _, it := range items {
			_, _ = fmt.Fprintf(out, "    %s  %s\n", shortFindingID(it.ID), it.Title)
			loc := formatLocation(it)
			meta := []string{}
			if loc != "" {
				meta = append(meta, "file: "+loc)
			}
			if it.Confidence != "" {
				meta = append(meta, "confidence: "+it.Confidence)
			}
			if it.AgentActionID != "" {
				meta = append(meta, "trajectory: "+shortFindingID(it.AgentActionID))
			}
			if len(meta) > 0 {
				_, _ = fmt.Fprint(out, "      ")
				for i, m := range meta {
					if i > 0 {
						_, _ = fmt.Fprint(out, "   ")
					}
					_, _ = fmt.Fprint(out, m)
				}
				_, _ = fmt.Fprintln(out)
			}
			emitted++
		}
	}
	if emitted == 0 {
		_, _ = fmt.Fprintln(out, "\nNo intent gaps reported.")
	}

	renderCoverageLine(out, res.CoverageSummary)
}

// findingSummary is the slice of a finding the local renderer needs.
type findingSummary struct {
	ID            string
	Kind          string
	Title         string
	Confidence    string
	File          string
	LineStart     int
	LineEnd       int
	AgentActionID string
}

// parseFindingsByKind unmarshals the findings array into the renderer's
// minimal view and groups by kind. Findings that fail to decode are
// skipped; analyzer and cache validation enforce the schema before
// rendering.
func parseFindingsByKind(raw json.RawMessage) map[string][]findingSummary {
	groups := map[string][]findingSummary{}
	if len(raw) == 0 {
		return groups
	}
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return groups
	}
	for _, m := range arr {
		s := findingSummary{}
		_ = json.Unmarshal(m["finding_id"], &s.ID)
		_ = json.Unmarshal(m["kind"], &s.Kind)
		_ = json.Unmarshal(m["title"], &s.Title)
		_ = json.Unmarshal(m["confidence"], &s.Confidence)
		switch s.Kind {
		case "under_impl":
			// under_impl can list multiple regions; render the first
			// as the primary location.
			var ei struct {
				Regions []struct {
					File  string  `json:"file"`
					Lines [][]int `json:"lines"`
				} `json:"ai_authored_regions_checked"`
			}
			_ = json.Unmarshal(m["observed_diff_evidence"], &ei)
			if len(ei.Regions) > 0 {
				s.File = ei.Regions[0].File
				if len(ei.Regions[0].Lines) > 0 && len(ei.Regions[0].Lines[0]) >= 2 {
					s.LineStart = ei.Regions[0].Lines[0][0]
					s.LineEnd = ei.Regions[0].Lines[0][1]
				}
			}
		case "deferred":
			var cs struct {
				File      string `json:"file"`
				LineRange []int  `json:"line_range"`
			}
			_ = json.Unmarshal(m["current_state"], &cs)
			s.File = cs.File
			if len(cs.LineRange) >= 2 {
				s.LineStart = cs.LineRange[0]
				s.LineEnd = cs.LineRange[1]
			}
			var act struct {
				ActionID string `json:"action_id"`
			}
			_ = json.Unmarshal(m["agent_action_citation"], &act)
			s.AgentActionID = act.ActionID
		case "unrequested":
			var d struct {
				File      string `json:"file"`
				LineRange []int  `json:"line_range"`
			}
			_ = json.Unmarshal(m["delivered"], &d)
			s.File = d.File
			if len(d.LineRange) >= 2 {
				s.LineStart = d.LineRange[0]
				s.LineEnd = d.LineRange[1]
			}
		}
		groups[s.Kind] = append(groups[s.Kind], s)
	}
	return groups
}

// renderCoverageLine prints compact coverage counts and drop reasons.
func renderCoverageLine(out io.Writer, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var cov map[string]any
	if err := json.Unmarshal(raw, &cov); err != nil {
		return
	}
	parts := []string{}
	if v, ok := numberFromCoverage(cov, "pr_commits_total"); ok {
		parts = append(parts, fmt.Sprintf("%d commit(s) analyzed", v))
	}
	if v, ok := numberFromCoverage(cov, "total_prompt_count"); ok {
		parts = append(parts, fmt.Sprintf("%d turn(s) captured", v))
	}
	if v, ok := numberFromCoverage(cov, "agent_actions_count"); ok {
		parts = append(parts, fmt.Sprintf("%d action(s) captured", v))
	}
	if v, ok := numberFromCoverage(cov, "findings_dropped"); ok && v > 0 {
		parts = append(parts, fmt.Sprintf("%d finding(s) dropped", v))
	}
	if len(parts) == 0 {
		return
	}
	_, _ = fmt.Fprintf(out, "\nCoverage: %s", joinComma(parts))
	if reasons, ok := cov["drop_reasons"].(map[string]any); ok && len(reasons) > 0 {
		keys := make([]string, 0, len(reasons))
		for k := range reasons {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		_, _ = fmt.Fprint(out, " (")
		for i, k := range keys {
			if i > 0 {
				_, _ = fmt.Fprint(out, ", ")
			}
			if n, ok := numberFromCoverage(reasons, k); ok {
				_, _ = fmt.Fprintf(out, "%s=%d", k, n)
			} else {
				_, _ = fmt.Fprint(out, k)
			}
		}
		_, _ = fmt.Fprint(out, ")")
	}
	_, _ = fmt.Fprintln(out)
}

func numberFromCoverage(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

func formatLocation(s findingSummary) string {
	if s.File == "" {
		return ""
	}
	if s.LineStart > 0 && s.LineEnd > 0 {
		return fmt.Sprintf("%s:%d-%d", s.File, s.LineStart, s.LineEnd)
	}
	return s.File
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func shortFindingID(id string) string {
	if len(id) > 10 {
		return id[:10] + "..."
	}
	return id
}

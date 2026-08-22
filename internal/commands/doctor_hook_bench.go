package commands

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/semanticash/cli/internal/doctor"
	"github.com/spf13/cobra"
)

// hookBenchRecord is the subset of a doctor bench record this report reads.
type hookBenchRecord struct {
	TS            string           `json:"ts"`
	Kind          string           `json:"kind"`
	Phase         string           `json:"phase"`
	SessionID     string           `json:"session_id"`
	TurnID        string           `json:"turn_id"`
	ToolUseID     string           `json:"tool_use_id"`
	DurationMS    int64            `json:"duration_ms"`
	Outcome       string           `json:"outcome"`
	PartialReason string           `json:"partial_reason"`
	StageLeafMS   map[string]int64 `json:"stage_leaf_ms"`
	UnaccountedMS int64            `json:"unaccounted_ms"`
}

type statTriple struct {
	P50 int64 `json:"p50_ms"`
	P95 int64 `json:"p95_ms"`
	Max int64 `json:"max_ms"`
}

type stageStat struct {
	Name  string `json:"name"`
	P50   int64  `json:"p50_ms"` // p50 among records where the stage is present
	P95   int64  `json:"p95_ms"`
	Count int    `json:"count"` // records that recorded this stage
}

// hookBenchSummary is the computed report, also the --json shape.
type hookBenchSummary struct {
	RecordingEnabled bool           `json:"recording_enabled"`
	Records          int            `json:"records"`
	Paired           int            `json:"paired"`
	UnpairedPre      int            `json:"unpaired_pre"`
	UnpairedPost     int            `json:"unpaired_post"`
	FirstTS          string         `json:"first_ts,omitempty"`
	LastTS           string         `json:"last_ts,omitempty"`
	Pre              statTriple     `json:"pre"`
	Post             statTriple     `json:"post"`
	Combined         statTriple     `json:"combined"`
	Stages           []stageStat    `json:"stages"`
	Outcomes         map[string]int `json:"outcomes"`
	PartialReasons   map[string]int `json:"partial_reasons,omitempty"`
}

func percentileInt(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func triple(xs []int64) statTriple {
	if len(xs) == 0 {
		return statTriple{}
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	return statTriple{P50: percentileInt(xs, 0.50), P95: percentileInt(xs, 0.95), Max: xs[len(xs)-1]}
}

// summarizeHookBench computes the report from tool-window records in JSONL
// order. Pre and post percentiles use every record. Combined pairs each pre
// with the next post for the same session, turn, and tool use. Repeated
// identities form distinct pairs, and leftover records are counted as unpaired.
func summarizeHookBench(recs []hookBenchRecord) hookBenchSummary {
	s := hookBenchSummary{Records: len(recs), Outcomes: map[string]int{}, PartialReasons: map[string]int{}}
	var preD, postD, combD []int64
	pendingPre := map[string][]int64{} // key -> FIFO queue of unmatched pre durations
	stageVals := map[string][]int64{}

	for i := range recs {
		r := &recs[i]
		if r.TS != "" {
			if s.FirstTS == "" || r.TS < s.FirstTS {
				s.FirstTS = r.TS
			}
			if r.TS > s.LastTS {
				s.LastTS = r.TS
			}
		}
		if r.Outcome != "" {
			s.Outcomes[r.Outcome]++
		}
		if r.PartialReason != "" {
			s.PartialReasons[r.PartialReason]++
		}
		key := r.SessionID + "|" + r.TurnID + "|" + r.ToolUseID
		switch r.Phase {
		case "pre":
			preD = append(preD, r.DurationMS)
			pendingPre[key] = append(pendingPre[key], r.DurationMS)
		case "post":
			postD = append(postD, r.DurationMS)
			if q := pendingPre[key]; len(q) > 0 {
				s.Paired++
				combD = append(combD, q[0]+r.DurationMS)
				pendingPre[key] = q[1:]
			} else {
				s.UnpairedPost++
			}
		}
		for name, ms := range r.StageLeafMS {
			stageVals[name] = append(stageVals[name], ms)
		}
	}
	for _, q := range pendingPre {
		s.UnpairedPre += len(q)
	}

	s.Pre, s.Post, s.Combined = triple(preD), triple(postD), triple(combD)
	for name, xs := range stageVals {
		t := triple(xs)
		s.Stages = append(s.Stages, stageStat{Name: name, P50: t.P50, P95: t.P95, Count: len(xs)})
	}
	sort.Slice(s.Stages, func(i, j int) bool {
		if s.Stages[i].P50 != s.Stages[j].P50 {
			return s.Stages[i].P50 > s.Stages[j].P50
		}
		return s.Stages[i].Name < s.Stages[j].Name
	})
	return s
}

// readHookBenchRecords reads tool-window records from a bench log. A missing
// file yields no records and no error.
func readHookBenchRecords(path string) ([]hookBenchRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []hookBenchRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var r hookBenchRecord
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue // tolerate foreign or truncated lines
		}
		if r.Kind == "toolwindow" {
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

// filterSince keeps records at or after now-dur. Records with an unparseable
// timestamp are dropped when a window is requested.
func filterSince(recs []hookBenchRecord, since string) ([]hookBenchRecord, error) {
	if strings.TrimSpace(since) == "" {
		return recs, nil
	}
	dur, err := time.ParseDuration(since)
	if err != nil {
		return nil, fmt.Errorf("invalid --since %q: %w", since, err)
	}
	cutoff := time.Now().Add(-dur)
	out := recs[:0:0]
	for _, r := range recs {
		ts, err := time.Parse(time.RFC3339Nano, r.TS)
		if err != nil {
			continue
		}
		if !ts.Before(cutoff) {
			out = append(out, r)
		}
	}
	return out, nil
}

func newDoctorHookBenchCmd(rootOpts *RootOptions) *cobra.Command {
	var (
		last   int
		since  string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:    "hook-bench",
		Short:  "Summarize tool-window hook latency from doctor bench records",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			repoPath := resolveDoctorRepo(rootOpts.RepoPath)
			if repoPath == "" {
				return fmt.Errorf("hook-bench: not inside a Git repository")
			}
			recs, err := readHookBenchRecords(doctor.BenchLogPath(repoPath))
			if err != nil {
				return err
			}
			if recs, err = filterSince(recs, since); err != nil {
				return err
			}
			if last > 0 && len(recs) > last {
				recs = recs[len(recs)-last:]
			}
			out := cmd.OutOrStdout()
			enabled := doctor.BenchEnabled(repoPath)
			if len(recs) == 0 {
				if asJSON {
					enc := json.NewEncoder(out)
					enc.SetIndent("", "  ")
					return enc.Encode(hookBenchSummary{RecordingEnabled: enabled, Outcomes: map[string]int{}})
				}
				writeHookBenchEmpty(out, repoPath, enabled)
				return nil
			}
			summary := summarizeHookBench(recs)
			summary.RecordingEnabled = enabled
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(summary)
			}
			writeHookBenchText(out, summary)
			return nil
		},
	}
	cmd.Flags().IntVar(&last, "last", 0, "Only the most recent N records")
	cmd.Flags().StringVar(&since, "since", "", "Only records within a window, e.g. 24h or 90m")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output as JSON")
	return cmd
}

func writeHookBenchEmpty(out io.Writer, repoPath string, enabled bool) {
	if enabled {
		_, _ = fmt.Fprintln(out, "Hook benchmark: recording is enabled but no tool-window records yet.")
		_, _ = fmt.Fprintln(out, "Run some agent Bash tools in this repo, then re-run this command.")
		return
	}
	enablePath := filepath.Join(filepath.Dir(doctor.BenchLogPath(repoPath)), "bench.enabled")
	_, _ = fmt.Fprintln(out, "Hook benchmark: recording is disabled, so there are no records.")
	_, _ = fmt.Fprintln(out, "Enable it (read-only diagnostics), then run some agent Bash tools:")
	_, _ = fmt.Fprintf(out, "  touch %s\n", enablePath)
	_, _ = fmt.Fprintln(out, "  # or set SEMANTICA_DOCTOR_BENCH=1")
}

func ms(v int64) string { return fmt.Sprintf("%dms", v) }

func writeHookBenchText(out io.Writer, s hookBenchSummary) {
	_, _ = fmt.Fprintf(out, "Hook benchmark: %d records, %d paired", s.Records, s.Paired)
	if s.UnpairedPre > 0 || s.UnpairedPost > 0 {
		_, _ = fmt.Fprintf(out, " (%d unpaired pre, %d unpaired post)", s.UnpairedPre, s.UnpairedPost)
	}
	_, _ = fmt.Fprintln(out)
	if s.FirstTS != "" {
		_, _ = fmt.Fprintf(out, "Range: %s .. %s\n", s.FirstTS, s.LastTS)
	}
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "%-10s %8s %8s %8s\n", "", "p50", "p95", "max")
	row := func(name string, t statTriple) {
		_, _ = fmt.Fprintf(out, "%-10s %8s %8s %8s\n", name, ms(t.P50), ms(t.P95), ms(t.Max))
	}
	row("Pre", s.Pre)
	row("Post", s.Post)
	row("Combined", s.Combined)

	if len(s.Stages) > 0 {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "Top stages (p50 when present):")
		limit := len(s.Stages)
		if limit > 8 {
			limit = 8
		}
		for _, st := range s.Stages[:limit] {
			_, _ = fmt.Fprintf(out, "  %-22s %7s p50  %7s p95  (n=%d)\n", st.Name, ms(st.P50), ms(st.P95), st.Count)
		}
	}

	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "Outcomes: %s\n", joinCounts(s.Outcomes))
	if len(s.PartialReasons) > 0 {
		_, _ = fmt.Fprintf(out, "Partial reasons: %s\n", joinCounts(s.PartialReasons))
	}
}

func joinCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%d %s", m[k], k)
	}
	return strings.Join(parts, ", ")
}

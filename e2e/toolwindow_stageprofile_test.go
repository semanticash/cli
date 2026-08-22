//go:build e2e

package e2e_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

type stageRecord struct {
	Kind          string           `json:"kind"`
	Phase         string           `json:"phase"`
	Outcome       string           `json:"outcome"`
	DurationMS    int64            `json:"duration_ms"`
	StageLeafMS   map[string]int64 `json:"stage_leaf_ms"`
	StageAggMS    map[string]int64 `json:"stage_agg_ms"`
	UnaccountedMS int64            `json:"unaccounted_ms"`
}

func readStageRecords(t *testing.T, path string) []stageRecord {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open bench log: %v", err)
	}
	defer func() { _ = f.Close() }()
	var out []stageRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var r stageRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("bad bench record: %v", err)
		}
		if r.Kind == "toolwindow" {
			out = append(out, r)
		}
	}
	return out
}

func leafSum(r stageRecord) int64 {
	var s int64
	for _, v := range r.StageLeafMS {
		s += v
	}
	return s
}

// reportStages prints stage percentiles and shares of median hook duration.
func reportStages(t *testing.T, label string, recs []stageRecord) {
	t.Helper()
	if len(recs) == 0 {
		t.Fatalf("%s: no records", label)
	}
	durs := make([]float64, len(recs))
	names := map[string]bool{}
	for i, r := range recs {
		durs[i] = float64(r.DurationMS)
		for k := range r.StageLeafMS {
			names["leaf:"+k] = true
		}
		for k := range r.StageAggMS {
			names["agg:"+k] = true
		}
	}
	names["unaccounted"] = true
	sort.Float64s(durs)
	durP50 := percentile(durs, 0.50)

	col := func(get func(stageRecord) int64) (p50, p95 float64) {
		xs := make([]float64, len(recs))
		for i, r := range recs {
			xs[i] = float64(get(r))
		}
		sort.Float64s(xs)
		return percentile(xs, 0.50), percentile(xs, 0.95)
	}

	keys := make([]string, 0, len(names))
	for k := range names {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	t.Logf("== %s: n=%d duration p50=%.0f p95=%.0f ms ==", label, len(recs), durP50, percentile(durs, 0.95))
	for _, k := range keys {
		var get func(stageRecord) int64
		switch {
		case k == "unaccounted":
			get = func(r stageRecord) int64 { return r.UnaccountedMS }
		case len(k) > 5 && k[:5] == "leaf:":
			name := k[5:]
			get = func(r stageRecord) int64 { return r.StageLeafMS[name] }
		default:
			name := k[4:]
			get = func(r stageRecord) int64 { return r.StageAggMS[name] }
		}
		p50, p95 := col(get)
		pct := 0.0
		if durP50 > 0 {
			pct = 100 * p50 / durP50
		}
		t.Logf("  %-28s p50=%6.1f p95=%6.1f  (%4.1f%% of duration p50)", k, p50, p95, pct)
	}
}

func checkStageInvariants(t *testing.T, label string, recs []stageRecord) {
	t.Helper()
	for i, r := range recs {
		if r.Outcome == "" {
			t.Errorf("%s[%d]: missing outcome (partial/failure must stay distinguishable)", label, i)
		}
		if r.UnaccountedMS < 0 {
			t.Errorf("%s[%d]: unaccounted is negative (%d)", label, i, r.UnaccountedMS)
		}
		// Allow one millisecond of truncation per leaf.
		recon := leafSum(r) + r.UnaccountedMS
		tol := int64(len(r.StageLeafMS)) + 2
		if diff := r.DurationMS - recon; diff < -tol || diff > tol {
			t.Errorf("%s[%d]: reconcile off by %d (tol %d): duration=%d leaf+unacct=%d", label, i, r.DurationMS-recon, tol, r.DurationMS, recon)
		}
		// Capture aggregates contain their leaf stages.
		if cb, ok := r.StageAggMS["capture_before"]; ok {
			wrapped := r.StageLeafMS["resolve_head"] + r.StageLeafMS["dirty_paths"] + r.StageLeafMS["hash"] + r.StageLeafMS["tree_write"]
			if cb+1 < wrapped {
				t.Errorf("%s[%d]: capture_before %d < wrapped leaves %d", label, i, cb, wrapped)
			}
		}
		if ca, ok := r.StageAggMS["capture_after"]; ok {
			wrapped := r.StageLeafMS["resolve_head"] + r.StageLeafMS["dirty_paths"] + r.StageLeafMS["hash"] + r.StageLeafMS["tree_write"] + r.StageLeafMS["delta"]
			if ca+1 < wrapped {
				t.Errorf("%s[%d]: capture_after %d < wrapped leaves %d", label, i, ca, wrapped)
			}
		}
	}
}

// TestToolWindowStageProfile verifies stage accounting over short hook cycles.
func TestToolWindowStageProfile(t *testing.T) {
	const cycles = 20

	run := func(t *testing.T, dirty int) (pre, post []stageRecord) {
		w := newBenchWorld(t)
		benchDir := filepath.Join(w.repo, ".semantica", "doctor")
		if err := os.MkdirAll(benchDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(benchDir, "bench.enabled"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < dirty; i++ {
			w.write(fmt.Sprintf("untracked%05d.txt", i), fmt.Sprintf("dirt %d\n", i))
		}
		for i := 0; i < cycles; i++ {
			w.cycle(i)
		}
		recs := readStageRecords(t, filepath.Join(benchDir, "bench.jsonl"))
		for _, r := range recs {
			switch r.Phase {
			case "pre":
				pre = append(pre, r)
			case "post":
				post = append(post, r)
			}
		}
		return pre, post
	}

	for _, tc := range []struct {
		name  string
		dirty int
	}{{"clean", 0}, {"dirty500", 500}} {
		t.Run(tc.name, func(t *testing.T) {
			pre, post := run(t, tc.dirty)
			if len(pre) == 0 || len(post) == 0 {
				t.Fatalf("missing records: pre=%d post=%d", len(pre), len(post))
			}
			// Pre and post records have separate aggregates.
			for _, r := range pre {
				if _, ok := r.StageAggMS["capture_after"]; ok {
					t.Error("pre record carries capture_after (pre/post combined?)")
				}
			}
			for _, r := range post {
				if _, ok := r.StageAggMS["capture_before"]; ok {
					t.Error("post record carries capture_before (pre/post combined?)")
				}
			}
			// Dirty pre-capture must include hashing.
			hashedPre := false
			for _, r := range pre {
				if r.StageLeafMS["hash"] > 0 {
					hashedPre = true
				}
			}
			if tc.dirty > 0 && !hashedPre {
				t.Error("dirty pre never recorded a hash stage")
			}

			checkStageInvariants(t, tc.name+"/pre", pre)
			checkStageInvariants(t, tc.name+"/post", post)
			reportStages(t, tc.name+"/pre", pre)
			reportStages(t, tc.name+"/post", post)
		})
	}
}

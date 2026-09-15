//go:build turnexperiment

package toolsnap

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTurnExperimentRealRepos reads explicit subjects and writes snapshots only
// to temporary stores. It launches no workload and assumes idle boundaries;
// external writers are neither identified nor declared complete.
func TestTurnExperimentRealRepos(t *testing.T) {
	config := os.Getenv("SEMANTICA_TURN_EXPERIMENT_REPOS")
	if config == "" {
		t.Skip("set SEMANTICA_TURN_EXPERIMENT_REPOS to a JSON array of repository_id/path pairs")
	}
	raw, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	var subjects []turnSubject
	if err := json.Unmarshal(raw, &subjects); err != nil {
		t.Fatal(err)
	}
	if len(subjects) == 0 {
		t.Fatal("no subjects")
	}
	storage := t.TempDir()
	type measurement struct {
		Concurrency int           `json:"concurrency"`
		Iteration   int           `json:"iteration"`
		ColdStore   bool          `json:"cold_store"`
		Start       time.Duration `json:"start_ns"`
		End         time.Duration `json:"end_ns"`
		StoredBytes int64         `json:"stored_bytes"`
		Samples     []turnSample  `json:"samples"`
	}
	var measurements []measurement
	// Each mode gets one new-store and 30 reused-store runs. Alternating order
	// limits systematic cache/load bias; no outliers are discarded.
	for i := 0; i < 31; i++ {
		modes := []int{1, len(subjects)}
		if i%2 != 0 {
			modes[0], modes[1] = modes[1], modes[0]
		}
		for _, concurrency := range modes {
			modeStorage := filepath.Join(storage, fmt.Sprint(concurrency))
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			turn, err := beginObservedTurn(ctx, "idle-benchmark", fmt.Sprint(i), modeStorage, subjects, concurrency)
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			finishObservedTurn(ctx, turn, "idle-benchmark", fmt.Sprint(i), true)
			cancel()
			var stored int64
			err = filepath.WalkDir(modeStorage, func(path string, d fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if d.IsDir() {
					return nil
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				stored += info.Size()
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			measurements = append(measurements, measurement{concurrency, i, i == 0, turn.StartTime, turn.EndTime, stored, turn.Samples})
			t.Logf("concurrency=%d iteration=%d cold_store=%v start=%s end=%s stored_bytes=%d", concurrency, i, i == 0, turn.StartTime, turn.EndTime, stored)
		}
	}
	report, err := json.MarshalIndent(measurements, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("SEMANTICA_TURN_EXPERIMENT_OUTPUT"); path != "" {
		if err := os.WriteFile(path, append(report, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	} else {
		t.Log(string(report))
	}
}

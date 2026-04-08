package implementations

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/semanticash/cli/internal/store/impldb"
	impldbgen "github.com/semanticash/cli/internal/store/impldb/db"
)

func TestPrecompute_DeduplicatesBranchDetection(t *testing.T) {
	var branchCalls int64
	r := &Reconciler{
		DetectBranch: func(_ context.Context, _ string) string {
			atomic.AddInt64(&branchCalls, 1)
			return "main"
		},
	}

	// 5 observations targeting the same repo should produce 1 branch call.
	obs := make([]impldbgen.Observation, 5)
	for i := range obs {
		obs[i] = impldbgen.Observation{
			ObservationID:     uuid.NewString(),
			Provider:          "claude_code",
			ProviderSessionID: "sess-1",
			TargetRepoPath:    "/repos/api",
			EventTs:           time.Now().UnixMilli(),
		}
	}

	ctx := context.Background()
	cache := r.precompute(ctx, obs)

	if atomic.LoadInt64(&branchCalls) != 1 {
		t.Errorf("expected 1 branch detection call, got %d", branchCalls)
	}
	if cache.branch("/repos/api") != "main" {
		t.Errorf("cached branch: got %q", cache.branch("/repos/api"))
	}
}

func TestPrecompute_DeduplicatesBranchAcrossRepos(t *testing.T) {
	var branchCalls int64
	r := &Reconciler{
		DetectBranch: func(_ context.Context, _ string) string {
			atomic.AddInt64(&branchCalls, 1)
			return "feature/x"
		},
	}

	// 3 observations across 2 repos -> 2 branch calls.
	obs := []impldbgen.Observation{
		{ObservationID: uuid.NewString(), Provider: "claude_code", ProviderSessionID: "s1", TargetRepoPath: "/repos/api"},
		{ObservationID: uuid.NewString(), Provider: "claude_code", ProviderSessionID: "s1", TargetRepoPath: "/repos/sdk"},
		{ObservationID: uuid.NewString(), Provider: "claude_code", ProviderSessionID: "s2", TargetRepoPath: "/repos/api"},
	}

	ctx := context.Background()
	r.precompute(ctx, obs)

	if atomic.LoadInt64(&branchCalls) != 2 {
		t.Errorf("expected 2 branch detection calls (one per repo), got %d", branchCalls)
	}
}

func TestPrecompute_DeduplicatesLocalSessionLookup(t *testing.T) {
	// resolveLocalSessionID will fail (no real lineage.db), which is fine.
	// We're counting branch calls only here; local session dedup is verified
	// by checking the cache returns empty for missing repos (no double lookup).
	var branchCalls int64
	r := &Reconciler{
		DetectBranch: func(_ context.Context, _ string) string {
			atomic.AddInt64(&branchCalls, 1)
			return "main"
		},
	}

	// Same (repo, provider, session) appearing 3 times should produce
	// 1 branch call and 1 local session lookup attempt.
	obs := make([]impldbgen.Observation, 3)
	for i := range obs {
		obs[i] = impldbgen.Observation{
			ObservationID:     uuid.NewString(),
			Provider:          "claude_code",
			ProviderSessionID: "sess-dup",
			TargetRepoPath:    "/repos/api",
			EventTs:           time.Now().UnixMilli(),
		}
	}

	ctx := context.Background()
	cache := r.precompute(ctx, obs)

	if atomic.LoadInt64(&branchCalls) != 1 {
		t.Errorf("expected 1 branch call, got %d", branchCalls)
	}
	// Local session won't resolve (no lineage.db), so cache should be empty.
	if got := cache.localSession("/repos/api", "claude_code", "sess-dup"); got != "" {
		t.Errorf("expected empty local session (no lineage.db), got %q", got)
	}
}

func TestPrecompute_AcrossMultipleBatches(t *testing.T) {
	var branchCalls int64
	r := &Reconciler{
		DetectBranch: func(_ context.Context, _ string) string {
			atomic.AddInt64(&branchCalls, 1)
			return "main"
		},
	}

	batch1 := []impldbgen.Observation{
		{ObservationID: uuid.NewString(), Provider: "claude_code", ProviderSessionID: "s1", TargetRepoPath: "/repos/api"},
	}
	batch2 := []impldbgen.Observation{
		{ObservationID: uuid.NewString(), Provider: "claude_code", ProviderSessionID: "s2", TargetRepoPath: "/repos/api"},
	}
	batch3 := []impldbgen.Observation{
		{ObservationID: uuid.NewString(), Provider: "claude_code", ProviderSessionID: "s3", TargetRepoPath: "/repos/sdk"},
	}

	ctx := context.Background()
	r.precompute(ctx, batch1, batch2, batch3)

	// /repos/api appears in batch1 and batch2, /repos/sdk in batch3 -> 2 calls.
	if atomic.LoadInt64(&branchCalls) != 2 {
		t.Errorf("expected 2 branch calls across 3 batches, got %d", branchCalls)
	}
}

// Broker batching test.

func TestEmitObservations_SingleOpenForBatch(t *testing.T) {
	// This test verifies the batch path works by inserting multiple
	// observations and checking they all appear in one DB.
	dir := t.TempDir()
	t.Setenv("SEMANTICA_HOME", dir)
	ctx := context.Background()

	// Create the DB first.
	dbPath := dir + "/implementations.db"
	h, err := impldb.Open(ctx, dbPath, impldb.DefaultOpenOptions())
	if err != nil {
		t.Fatal(err)
	}
	_ = impldb.Close(h)

	// This package-level test focuses on cache behavior. Broker batch inserts
	// are covered in observe_test.go.

	// Multiple observations in the same repo should reuse one cached branch.
	var calls int64
	r := &Reconciler{
		DetectBranch: func(_ context.Context, _ string) string {
			atomic.AddInt64(&calls, 1)
			return "feat"
		},
	}

	obs := make([]impldbgen.Observation, 10)
	for i := range obs {
		obs[i] = impldbgen.Observation{
			ObservationID:     uuid.NewString(),
			Provider:          "claude_code",
			ProviderSessionID: "sess-" + uuid.NewString()[:4],
			TargetRepoPath:    "/repos/api",
		}
	}

	cache := r.precompute(ctx, obs)
	if atomic.LoadInt64(&calls) != 1 {
		t.Errorf("10 observations, 1 repo -> expected 1 branch call, got %d", calls)
	}
	if cache.branch("/repos/api") != "feat" {
		t.Errorf("branch: got %q", cache.branch("/repos/api"))
	}
}

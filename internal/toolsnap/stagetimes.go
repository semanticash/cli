package toolsnap

import (
	"context"
	"sync"
	"time"
)

// stageKind distinguishes summable leaves from aggregate wrappers.
type stageKind int

const (
	stageLeaf stageKind = iota
	stageAggregate
)

// StageTimes collects diagnostic capture timings. Without a context collector,
// stage measurement does not read the clock.
type StageTimes struct {
	mu   sync.Mutex
	leaf map[string]time.Duration
	agg  map[string]time.Duration
}

type stageTimesKey struct{}

// WithStageTimes attaches a new timing collector to ctx.
func WithStageTimes(ctx context.Context) (context.Context, *StageTimes) {
	st := &StageTimes{leaf: map[string]time.Duration{}, agg: map[string]time.Duration{}}
	return context.WithValue(ctx, stageTimesKey{}, st), st
}

func stageTimesFrom(ctx context.Context) *StageTimes {
	st, _ := ctx.Value(stageTimesKey{}).(*StageTimes)
	return st
}

func (st *StageTimes) record(name string, kind stageKind, d time.Duration) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if kind == stageAggregate {
		st.agg[name] += d
		return
	}
	st.leaf[name] += d
}

// LeafMillis returns the accumulated leaf-stage durations in whole milliseconds.
func (st *StageTimes) LeafMillis() map[string]int64 { return millisCopy(st, st.leaf) }

// AggregateMillis returns wrapper durations in whole milliseconds.
func (st *StageTimes) AggregateMillis() map[string]int64 { return millisCopy(st, st.agg) }

// LeafTotal returns the summed leaf durations, for computing unaccounted time.
func (st *StageTimes) LeafTotal() time.Duration {
	st.mu.Lock()
	defer st.mu.Unlock()
	var total time.Duration
	for _, d := range st.leaf {
		total += d
	}
	return total
}

func millisCopy(st *StageTimes, src map[string]time.Duration) map[string]int64 {
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]int64, len(src))
	for k, d := range src {
		out[k] = d.Milliseconds()
	}
	return out
}

// noopStop avoids allocation and clock reads when timing is disabled.
var noopStop = func() {}

// MeasureStage starts a leaf timer. The returned function stops it.
func MeasureStage(ctx context.Context, name string) func() {
	return measureStage(ctx, name, stageLeaf)
}

// measureStage returns a no-op when ctx has no collector.
func measureStage(ctx context.Context, name string, kind stageKind) func() {
	st := stageTimesFrom(ctx)
	if st == nil {
		return noopStop
	}
	start := time.Now()
	return func() { st.record(name, kind, time.Since(start)) }
}

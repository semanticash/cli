package implementations

import "time"

const (
	// DormancyTimeout is how long an implementation can be idle before
	// transitioning from active to dormant.
	DormancyTimeout = 60 * time.Minute

	// ReconcileBatch is the maximum number of observations processed per
	// reconciliation pass.
	ReconcileBatch = 100

	// MaxRetryAttempts is how many times a failed observation is retried
	// before it's considered permanently failed.
	MaxRetryAttempts = 3

	// DeferMaxAttempts is how many reconciliation cycles a child observation
	// waits for its parent before being processed as standalone.
	DeferMaxAttempts int64 = 1
)

// ReconcileResult summarizes what happened during a reconciliation pass.
type ReconcileResult struct {
	MarkedDormant    int64
	Processed        int
	DeferredResolved int
	Retried          int
	Errors           []error
}

// AttachCommitInput contains the parameters for linking a commit to an
// implementation.
type AttachCommitInput struct {
	RepoPath   string
	CommitHash string
}

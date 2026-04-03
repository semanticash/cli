package util

import "path/filepath"

const PreCommitCheckpointFile = ".pre-commit-checkpoint"
const CommitAttributionSummaryFile = ".commit-attribution-summary"

func PreCommitCheckpointPath(semDir string) string {
	return filepath.Join(semDir, PreCommitCheckpointFile)
}

func CommitAttributionSummaryPath(semDir string) string {
	return filepath.Join(semDir, CommitAttributionSummaryFile)
}

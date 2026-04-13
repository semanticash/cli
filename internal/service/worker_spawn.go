package service

import (
	"os"
	"os/exec"

	"github.com/semanticash/cli/internal/platform"
	"github.com/semanticash/cli/internal/util"
)

// spawnAutoPlaybook launches `semantica _auto-playbook` as a detached process.
func spawnAutoPlaybook(semDir, checkpointID, commitHash, repoRoot string) {
	exe, err := os.Executable()
	if err != nil {
		exe = "semantica"
	}

	logFile, err := util.OpenWorkerLog(semDir)
	if err != nil {
		wlog("worker: auto-playbook: open log failed: %v\n", err)
		return
	}

	cmd := exec.Command(exe, "_auto-playbook",
		"--checkpoint", checkpointID,
		"--commit", commitHash,
		"--repo", repoRoot,
	)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	platform.DetachProcess(cmd)

	if err := cmd.Start(); err != nil {
		wlog("worker: auto-playbook: spawn failed: %v\n", err)
		_ = logFile.Close()
		return
	}

	_ = logFile.Close()
	wlog("worker: auto-playbook spawned for checkpoint %s\n", checkpointID)
}

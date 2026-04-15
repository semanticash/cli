//go:build unix

package platform

import (
	"os/exec"
	"syscall"
)

// DetachProcess configures cmd to run in a new session, detached from
// the parent. The child survives the parent exiting.
func DetachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// SetProcessGroup configures cmd to run in a new process group.
func SetProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// HideWindow is a no-op on Unix. On Windows it suppresses console
// window creation for subprocess invocations from detached workers.
func HideWindow(_ *exec.Cmd) {}

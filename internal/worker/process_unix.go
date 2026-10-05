//go:build !windows

package worker

import (
	"os/exec"
	"syscall"
	"time"
)

func setupProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminate asks the browser process to shut down gracefully. Signalling only
// the parent lets Chrome flush profile state (cookies, localStorage) before it
// exits; killing the whole group first would lose recent writes.
func terminate(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(cmd.Process.Pid, syscall.SIGTERM)
}

func kill(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// processAlive reports whether a pid exists. Used for stale profile-lock
// detection.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

const gracefulStopWait = 5 * time.Second

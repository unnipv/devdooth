//go:build windows

package worker

import (
	"os/exec"
	"time"
)

func setupProcessGroup(cmd *exec.Cmd) {}

func terminate(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func kill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func processAlive(pid int) bool { return false }

const gracefulStopWait = 5 * time.Second

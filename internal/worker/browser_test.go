package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A browser probe must return within its timeout even when the process spawns a
// child that inherits stdout and outlives it. CommandContext alone kills only
// the direct process, so Output can otherwise block until that child exits.
func TestBrowserVersionTimeoutIsBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell script")
	}
	script := filepath.Join(t.TempDir(), "hang.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 20 &\nwait\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	start := time.Now()
	got := browserVersion(script)
	elapsed := time.Since(start)

	if got != "" {
		t.Fatalf("expected empty version, got %q", got)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("probe took %s; it must return shortly after the 5s timeout", elapsed)
	}
}

// Even when the direct process exits quickly, a child that inherited stdout must
// not keep Output blocked. This specifically requires WaitDelay: process-group
// cancellation alone does not help, because no timeout fires.
func TestBrowserVersionWaitDelayOnEarlyExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a POSIX shell script")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "early-exit.sh")
	body := "#!/bin/sh\nsleep 20 &\necho $! > " + pidFile + "\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = exec.Command("kill", "-9", strconv.Itoa(pid)).Run()
			}
		}
	})

	start := time.Now()
	got := browserVersion(script)
	elapsed := time.Since(start)

	if got != "" {
		t.Fatalf("expected empty version, got %q", got)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("probe took %s; WaitDelay must unblock Output shortly after the parent exits", elapsed)
	}
}

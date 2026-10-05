package worker

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/unnipv/devdooth/internal/protocol"
)

// browserCandidates returns candidate executables in priority order for the
// current platform. Devdooth uses browsers already installed on the machine;
// it does not download or redistribute them.
func browserCandidates() []protocol.Browser {
	var out []protocol.Browser

	type candidate struct {
		name string
		path string
	}
	var candidates []candidate

	switch runtime.GOOS {
	case "darwin":
		candidates = []candidate{
			{"chrome", "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"},
			{"chrome", "/Applications/Google Chrome Beta.app/Contents/MacOS/Google Chrome Beta"},
			{"chromium", "/Applications/Chromium.app/Contents/MacOS/Chromium"},
			{"chrome", "google-chrome"},
			{"chrome", "chrome"},
			{"chromium", "chromium"},
		}
	case "windows":
		candidates = []candidate{
			{"chrome", `C:\Program Files\Google\Chrome\Application\chrome.exe`},
			{"chrome", `C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`},
			{"msedge", `C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`},
		}
	default:
		candidates = []candidate{
			{"chromium", "/usr/bin/chromium"},
			{"chromium", "/usr/bin/chromium-browser"},
			{"chrome", "/usr/bin/google-chrome"},
			{"chrome", "/usr/bin/google-chrome-stable"},
			{"chrome", "google-chrome"},
			{"chrome", "chrome"},
			{"chromium", "chromium"},
			{"chromium", "chromium-browser"},
		}
	}

	seen := map[string]bool{}
	for _, c := range candidates {
		path := c.path
		if !filepath.IsAbs(path) {
			if p, err := exec.LookPath(path); err == nil {
				path = p
			} else {
				continue
			}
		} else if _, err := os.Stat(path); err != nil {
			continue
		}
		if seen[c.name] {
			continue
		}
		seen[c.name] = true
		out = append(out, protocol.Browser{Name: c.name, Path: path, Version: browserVersion(path)})
	}
	return out
}

func browserVersion(path string) string {
	// Bound the probe. CommandContext kills only the direct process, so kill the
	// whole process group and set WaitDelay: after cancellation Go waits at most
	// WaitDelay for I/O to drain, then closes the pipes and lets Output return.
	// Without WaitDelay, a child that inherits stdout could block Output
	// indefinitely (exec.Cmd is otherwise unsafe to touch concurrently).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	setupProcessGroup(cmd)
	cmd.Cancel = func() error {
		kill(cmd)
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// launchArgs builds the Chrome command line. The debug port is 0 so the OS
// assigns a free port, which is then read from DevToolsActivePort inside the
// browser's user-data directory. That keeps the endpoint loopback-only and
// avoids fixed-port collisions between concurrent leases.
func launchArgs(userDataDir string, headful, noSandbox bool, startURL string) []string {
	args := []string{
		"--remote-debugging-port=0",
		"--user-data-dir=" + userDataDir,
		"--remote-allow-origins=*",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-sync",
		"--metrics-recording-only",
		"--password-store=basic",
		"--use-mock-keychain",
	}
	if noSandbox {
		args = append(args, "--no-sandbox")
	}
	if !headful {
		args = append(args, "--headless=new", "--disable-gpu")
	}
	if runtime.GOOS == "linux" {
		args = append(args, "--disable-dev-shm-usage")
	}
	if startURL != "" {
		args = append(args, startURL)
	}
	return args
}

// tailWriter keeps the last max bytes written to it. Browser stderr is captured
// into one of these so a failed launch can report why Chrome refused to start.
type tailWriter struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func newTailWriter(max int) *tailWriter { return &tailWriter{max: max} }

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

// prepareProfileDir removes files a previous browser run leaves behind in a
// persistent profile. A stale DevToolsActivePort would otherwise be read as
// the current debug port on the next lease, and a stale singleton lock can
// make Chrome exit immediately. We hold the worker's profile lock, so no
// Devdooth browser is using this directory.
func prepareProfileDir(dir string) {
	for _, name := range []string{"DevToolsActivePort", "SingletonLock", "SingletonSocket", "SingletonCookie"} {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// waitForDevTools reads the port and browser path that Chrome writes to
// <user-data-dir>/DevToolsActivePort.
func waitForDevTools(userDataDir string, timeout time.Duration) (int, string, error) {
	file := filepath.Join(userDataDir, "DevToolsActivePort")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f, err := os.Open(file)
		if err == nil {
			sc := bufio.NewScanner(f)
			var lines []string
			for sc.Scan() {
				lines = append(lines, strings.TrimSpace(sc.Text()))
			}
			f.Close()
			if len(lines) >= 2 && lines[0] != "" {
				port, perr := strconv.Atoi(lines[0])
				if perr == nil && port > 0 {
					return port, lines[1], nil
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, "", fmt.Errorf("browser did not expose a debug port within %s", timeout)
}

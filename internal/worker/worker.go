// Package worker implements the Devdooth worker: a lightweight process that
// dials out to a coordinator, launches browsers, and relays CDP traffic for
// leases assigned to it.
//
// The worker never listens on an inbound port. All connections are outbound.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/unnipv/devdooth/internal/protocol"
	"github.com/unnipv/devdooth/internal/relay"
)

// Options configures a Worker.
type Options struct {
	CoordinatorURL string // http(s):// or ws(s):// coordinator base
	Token          string // static worker token (development)
	EnrollToken    string // single-use enrollment token
	DeviceToken    string // device token, if already known
	Name           string
	DataDir        string
	MaxSlots       int
	Headful        bool
	Browser        string   // preferred browser name, optional
	Profiles       []string // persistent profile names to create/advertise
	NoSandbox      bool     // pass --no-sandbox (containers, some CI)
	Logger         *log.Logger
}

// Worker is a Devdooth worker.
type Worker struct {
	opts        Options
	log         *log.Logger
	generation  string
	browsers    []protocol.Browser
	deviceToken string

	mu           sync.Mutex
	leases       map[string]*running
	profileLocks map[string]string // profile name -> lease id

	send   chan []byte
	closed chan struct{}
}

const (
	controlWriteTimeout = 30 * time.Second
	controlReadTimeout  = 90 * time.Second
	tunnelDialTimeout   = 15 * time.Second
	launchTimeout       = 40 * time.Second
)

// New creates a worker.
func New(opts Options) *Worker {
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.DataDir == "" {
		home, _ := os.UserHomeDir()
		opts.DataDir = filepath.Join(home, ".devdooth")
	}
	if opts.MaxSlots <= 0 {
		opts.MaxSlots = 1
	}
	return &Worker{
		opts:         opts,
		log:          opts.Logger,
		generation:   randomID(),
		browsers:     browserCandidates(),
		leases:       make(map[string]*running),
		profileLocks: make(map[string]string),
	}
}

func (w *Worker) logf(format string, args ...any) { w.log.Printf(format, args...) }

// Browsers returns the browsers discovered on this machine.
func (w *Worker) Browsers() []protocol.Browser { return w.browsers }

// Run connects to the coordinator and reconnects until ctx is done.
func (w *Worker) Run(ctx context.Context) error {
	if len(w.browsers) == 0 {
		return errors.New("no supported Chromium-family browser found on this machine")
	}
	if err := os.MkdirAll(w.profilesDir(), 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	if err := w.resolveAuth(ctx); err != nil {
		return err
	}
	for _, p := range w.opts.Profiles {
		if p == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Join(w.profilesDir(), p), 0o700); err != nil {
			return fmt.Errorf("create profile %q: %w", p, err)
		}
	}
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := w.session(ctx)
		w.stopAll()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		w.logf("coordinator connection lost (%v); retrying in %s", err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (w *Worker) session(ctx context.Context) error {
	dialURL, err := toWS(w.opts.CoordinatorURL, "/v1/worker/connect")
	if err != nil {
		return err
	}
	hdr := http.Header{"Authorization": {"Bearer " + w.authToken()}}

	// Tie the raw socket to cancellation. Gorilla reads the HTTP upgrade
	// response directly from the connection, so a cancelled DialContext alone
	// would not interrupt a stalled handshake; closing the raw conn does, and it
	// also interrupts the subsequent blocked reads.
	dialDone := make(chan struct{})
	defer close(dialDone)
	netDialer := &net.Dialer{Timeout: 10 * time.Second}
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 15 * time.Second
	dialer.NetDialContext = func(dialCtx context.Context, network, addr string) (net.Conn, error) {
		raw, err := netDialer.DialContext(dialCtx, network, addr)
		if err != nil {
			return nil, err
		}
		go func() {
			select {
			case <-ctx.Done():
				raw.Close()
			case <-dialDone:
			}
		}()
		return raw, nil
	}

	conn, _, err := dialer.DialContext(ctx, dialURL, hdr)
	if err != nil {
		return err
	}
	defer conn.Close()

	w.send = make(chan []byte, 64)
	w.closed = make(chan struct{})
	defer close(w.closed)

	go w.writer(conn)

	if err := w.sendMessage(protocol.Hello{
		Type:       protocol.TypeHello,
		Name:       w.opts.Name,
		Version:    Version,
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		Headful:    w.headfulCapable(),
		Browsers:   w.browsers,
		Profiles:   w.listProfiles(),
		MaxSlots:   w.opts.MaxSlots,
		Generation: w.generation,
	}); err != nil {
		return err
	}
	w.logf("connected to coordinator as %q", w.opts.Name)

	conn.SetReadLimit(1 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(controlReadTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(controlReadTimeout))
	})

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		_ = conn.SetReadDeadline(time.Now().Add(controlReadTimeout))
		w.handleMessage(data)
	}
}

func (w *Worker) writer(conn *websocket.Conn) {
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case msg := <-w.send:
			_ = conn.SetWriteDeadline(time.Now().Add(controlWriteTimeout))
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				conn.Close()
				return
			}
		case <-ticker.C:
			_ = conn.SetWriteDeadline(time.Now().Add(controlWriteTimeout))
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(controlWriteTimeout)); err != nil {
				conn.Close()
				return
			}
		case <-w.closed:
			return
		}
	}
}

func (w *Worker) sendMessage(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	select {
	case w.send <- b:
		return nil
	case <-w.closed:
		return errors.New("not connected")
	case <-time.After(5 * time.Second):
		return errors.New("send buffer full")
	}
}

func (w *Worker) handleMessage(data []byte) {
	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		w.logf("coordinator sent malformed message")
		return
	}
	switch env.Type {
	case protocol.TypeLaunch:
		var m protocol.Launch
		if json.Unmarshal(data, &m) == nil {
			go w.handleLaunch(m)
		}
	case protocol.TypeRelease:
		var m protocol.Release
		if json.Unmarshal(data, &m) == nil {
			go w.stopLease(m.LeaseID)
		}
	case protocol.TypeOpenTunnel:
		var m protocol.OpenTunnel
		if json.Unmarshal(data, &m) == nil {
			go w.handleTunnel(m)
		}
	}
}

// ---------------------------------------------------------------- browser lifecycle

type running struct {
	leaseID      string
	cmd          *exec.Cmd
	port         int
	wsPath       string
	profile      string // persistent profile name, empty for ephemeral
	ephemeralDir string
	done         chan struct{}

	onceOnce sync.Once
	ttl      *time.Timer
	w        *Worker
}

func (w *Worker) handleLaunch(m protocol.Launch) {
	rb, err := w.startBrowser(m)
	if err != nil {
		w.logf("lease %s launch failed: %v", m.LeaseID, err)
		_ = w.sendMessage(protocol.LaunchFail{Type: protocol.TypeLaunchFail, LeaseID: m.LeaseID, Error: err.Error()})
		return
	}
	w.mu.Lock()
	w.leases[m.LeaseID] = rb
	w.mu.Unlock()
	w.logf("lease %s ready (%s)", m.LeaseID, w.browserLabel(rb))
	_ = w.sendMessage(protocol.Ready{Type: protocol.TypeReady, LeaseID: m.LeaseID})
}

func (w *Worker) browserLabel(rb *running) string {
	if rb.profile != "" {
		return "profile " + rb.profile
	}
	return "ephemeral profile"
}

func (w *Worker) startBrowser(m protocol.Launch) (*running, error) {
	exe, _, err := w.pickBrowser(m.Browser)
	if err != nil {
		return nil, err
	}

	var userDataDir, profile string
	if m.Profile != "" {
		if !w.lockProfile(m.Profile, m.LeaseID) {
			return nil, fmt.Errorf("profile %q is already in use", m.Profile)
		}
		profile = m.Profile
		userDataDir = filepath.Join(w.profilesDir(), m.Profile)
		if err := os.MkdirAll(userDataDir, 0o700); err != nil {
			w.unlockProfile(profile)
			return nil, fmt.Errorf("prepare profile: %w", err)
		}
	} else {
		userDataDir = filepath.Join(w.opts.DataDir, "tmp", m.LeaseID)
		if err := os.MkdirAll(userDataDir, 0o700); err != nil {
			return nil, fmt.Errorf("prepare temporary profile: %w", err)
		}
	}

	cmd := exec.Command(exe, launchArgs(userDataDir, m.Headful, w.opts.NoSandbox, m.StartURL)...)
	setupProcessGroup(cmd)
	prepareProfileDir(userDataDir)
	logs := newTailWriter(8 << 10)
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		w.cleanupFailedStart(userDataDir, profile)
		return nil, fmt.Errorf("start browser: %w", err)
	}

	port, wsPath, derr := waitForDevTools(userDataDir, launchTimeout)
	if derr != nil {
		terminate(cmd)
		time.Sleep(500 * time.Millisecond)
		kill(cmd)
		_ = cmd.Wait()
		w.cleanupFailedStart(userDataDir, profile)
		if detail := logs.String(); detail != "" {
			return nil, fmt.Errorf("%w: %s", derr, firstLine(detail))
		}
		return nil, derr
	}

	rb := &running{
		leaseID: m.LeaseID, cmd: cmd, port: port, wsPath: wsPath,
		profile: profile, done: make(chan struct{}), w: w,
	}
	if profile == "" {
		rb.ephemeralDir = userDataDir
	}
	if m.TTLSeconds > 0 {
		rb.ttl = time.AfterFunc(time.Duration(m.TTLSeconds)*time.Second, func() {
			w.logf("lease %s reached its TTL; stopping browser", m.LeaseID)
			w.stopLease(m.LeaseID)
		})
	}
	go rb.monitor()
	return rb, nil
}

func (w *Worker) cleanupFailedStart(userDataDir, profile string) {
	if profile != "" {
		w.unlockProfile(profile)
		return
	}
	_ = os.RemoveAll(userDataDir)
}

func (w *Worker) pickBrowser(requested string) (string, string, error) {
	if len(w.browsers) == 0 {
		return "", "", errors.New("no supported browser found on this worker")
	}
	want := requested
	if want == "" {
		want = w.opts.Browser
	}
	if want == "" {
		return w.browsers[0].Path, w.browsers[0].Name, nil
	}
	for _, b := range w.browsers {
		if b.Name == want {
			return b.Path, b.Name, nil
		}
	}
	return "", "", fmt.Errorf("browser %q is not available on this worker", want)
}

func (r *running) monitor() {
	_ = r.cmd.Wait()
	r.finish()
}

func (r *running) finish() {
	r.onceOnce.Do(func() {
		if r.ttl != nil {
			r.ttl.Stop()
		}
		if r.ephemeralDir != "" {
			removeAllWithRetry(r.ephemeralDir)
		}
		if r.profile != "" {
			r.w.unlockProfile(r.profile)
		}
		r.w.mu.Lock()
		if r.w.leases[r.leaseID] == r {
			delete(r.w.leases, r.leaseID)
		}
		r.w.mu.Unlock()
		close(r.done)
		_ = r.w.sendMessage(protocol.Released{Type: protocol.TypeReleased, LeaseID: r.leaseID})
	})
}

func (w *Worker) stopLease(leaseID string) {
	w.mu.Lock()
	rb := w.leases[leaseID]
	w.mu.Unlock()
	if rb == nil {
		return
	}
	rb.stop()
}

func (r *running) stop() {
	// Ask the browser to close cleanly first. SIGTERM alone can exit Chrome
	// before it flushes cookies and localStorage to the profile, which breaks
	// "log in once, reuse later". Browser.close is a lifecycle command; the
	// worker does not interpret page traffic.
	requestBrowserClose(r.port, r.wsPath)
	select {
	case <-r.done:
		return
	case <-time.After(gracefulStopWait):
	}
	terminate(r.cmd)
	select {
	case <-r.done:
		return
	case <-time.After(gracefulStopWait):
	}
	kill(r.cmd)
	select {
	case <-r.done:
	case <-time.After(3 * time.Second):
	}
}

// requestBrowserClose sends Browser.close over the browser's own loopback
// debug endpoint and waits (bounded) for Chrome to finish shutting down.
func requestBrowserClose(port int, wsPath string) {
	dialer := websocket.Dialer{HandshakeTimeout: gracefulStopWait}
	conn, _, err := dialer.Dial(fmt.Sprintf("ws://127.0.0.1:%d%s", port, wsPath), nil)
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(gracefulStopWait))
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"id":1,"method":"Browser.close"}`)); err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(gracefulStopWait))
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (w *Worker) stopAll() {
	w.mu.Lock()
	all := make([]*running, 0, len(w.leases))
	for _, rb := range w.leases {
		all = append(all, rb)
	}
	w.mu.Unlock()
	// Stop browsers concurrently so shutdown time does not scale with slot count.
	var wg sync.WaitGroup
	for _, rb := range all {
		wg.Add(1)
		go func(rb *running) {
			defer wg.Done()
			rb.stop()
		}(rb)
	}
	wg.Wait()
}

// ---------------------------------------------------------------- tunnel

func (w *Worker) handleTunnel(m protocol.OpenTunnel) {
	w.mu.Lock()
	rb := w.leases[m.LeaseID]
	w.mu.Unlock()
	if rb == nil {
		w.logf("tunnel requested for unknown lease %s", m.LeaseID)
		return
	}

	tunnelURL, err := toWS(w.opts.CoordinatorURL, "/v1/tunnel/"+m.AttachmentID)
	if err != nil {
		w.logf("tunnel: %v", err)
		return
	}
	hdr := http.Header{"Authorization": {"Bearer " + m.TunnelToken}}
	dialer := websocket.Dialer{HandshakeTimeout: tunnelDialTimeout}
	coord, _, err := dialer.Dial(tunnelURL, hdr)
	if err != nil {
		w.logf("tunnel dial failed: %v", err)
		return
	}
	defer coord.Close()

	browserURL := fmt.Sprintf("ws://127.0.0.1:%d%s", rb.port, rb.wsPath)
	browser, _, err := dialer.Dial(browserURL, nil)
	if err != nil {
		w.logf("browser dial failed: %v", err)
		return
	}
	defer browser.Close()

	_ = relay.Pipe(coord, browser)
}

// ---------------------------------------------------------------- profiles

func (w *Worker) profilesDir() string { return filepath.Join(w.opts.DataDir, "profiles") }

func (w *Worker) listProfiles() []string {
	entries, err := os.ReadDir(w.profilesDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

func (w *Worker) lockProfile(name, leaseID string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, busy := w.profileLocks[name]; busy {
		return false
	}
	w.profileLocks[name] = leaseID
	return true
}

func (w *Worker) unlockProfile(name string) {
	w.mu.Lock()
	delete(w.profileLocks, name)
	w.mu.Unlock()
}

// removeAllWithRetry deletes an ephemeral profile directory. Chrome helpers can
// still be flushing files for a moment after the main process exits, so a
// single RemoveAll can fail with ENOTEMPTY.
// ponytail: fixed retry budget; a leaked temp dir is harmless and reaped on reboot.
func removeAllWithRetry(dir string) {
	for i := 0; i < 10; i++ {
		if err := os.RemoveAll(dir); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// ---------------------------------------------------------------- helpers

func (w *Worker) headfulCapable() bool {
	switch runtime.GOOS {
	case "darwin", "windows":
		return true
	default:
		return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	}
}

// toWS converts an HTTP(S) coordinator base and a path into a WebSocket URL.
func toWS(base, path string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("coordinator URL must be http(s) or ws(s), got %q", base)
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u.String(), nil
}

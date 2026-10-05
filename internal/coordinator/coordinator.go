// Package coordinator implements the Devdooth control plane.
//
// It owns node registry, lease allocation, and the relay between a client's
// CDP connection and a worker's outbound tunnel. It never interprets CDP.
package coordinator

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/unnipv/devdooth/internal/protocol"
)

// Lease states.
const (
	StateStarting = "starting"
	StateReady    = "ready"
	StateStopping = "stopping"
	StateClosed   = "closed"
	StateFailed   = "failed"
	StateExpired  = "expired"
)

// Options configures a Coordinator.
type Options struct {
	AdminToken  string
	WorkerToken string
	PublicURL   string
	// Store enables durable device identity and enrollment. When nil, only the
	// static WorkerToken is accepted (development mode).
	Store  *Store
	Logger *log.Logger
	// Now is injectable for tests.
	Now func() time.Time
}

// Coordinator is the Devdooth control plane.
type Coordinator struct {
	opts  Options
	store *Store
	log   *log.Logger
	now   func() time.Time

	mu          sync.Mutex
	nodes       map[string]*Node
	leases      map[string]*Lease
	attachments map[string]*attachment
	// revokedDevices closes the check-then-register race: a device revoked
	// between the WebSocket upgrade and its hello cannot register.
	revokedDevices map[string]struct{}
	// relays tracks active CDP relays per session so revocation can drop them.
	relays   map[*Node]map[uint64]func()
	relaySeq uint64

	upgrader websocket.Upgrader
}

// Node is a connected worker.
type Node struct {
	ID         string
	Name       string
	DeviceID   string // set when the worker authenticated with a device token
	OS         string
	Arch       string
	Headful    bool
	Browsers   []protocol.Browser
	Profiles   []string
	MaxSlots   int
	Generation string

	conn   *websocket.Conn
	send   chan []byte
	closed chan struct{}
	once   sync.Once

	// active is the number of reserved or running leases on this node.
	active int

	online   bool
	lastSeen time.Time
	// dead is set under the coordinator lock when the session is finished, so
	// late relay registrations are refused.
	dead bool
}

// Lease is one reserved browser slot.
type Lease struct {
	ID      string
	NodeID  string
	node    *Node // the exact control session that owns this lease
	Browser string
	Profile string
	Headful bool
	Token   string
	State   string
	Err     string
	Created time.Time
	Expires time.Time

	// attachMu guards single-controller and connection bookkeeping.
	attachMu   sync.Mutex
	attached   bool
	paused     bool
	attachment *attachment
	reserved   bool
	done       chan struct{}
	doneOnce   sync.Once
	stopped    chan struct{}
	stopOnce   sync.Once
	expireTmr  *time.Timer
}

type attachment struct {
	id          string
	leaseID     string
	node        *Node
	tunnelToken string
	created     time.Time

	mu        sync.Mutex
	closed    bool
	delivered bool
	tunnel    *websocket.Conn
	client    *websocket.Conn
	ready     chan struct{}
	readyOnce sync.Once
}

// setClient records the controller connection so a pause can detach it.
func (a *attachment) setClient(conn *websocket.Conn) {
	a.mu.Lock()
	a.client = conn
	a.mu.Unlock()
}

// dropController closes the controller connection, leaving the browser running
// on the worker. Used when a human takes over a headful session.
func (a *attachment) dropController() {
	a.mu.Lock()
	client := a.client
	a.mu.Unlock()
	if client != nil {
		client.Close()
	}
}

// deliver hands the worker's tunnel socket to the attachment. It returns false
// if the attachment was already cancelled or already has a tunnel, in which
// case the caller must close the socket.
func (a *attachment) deliver(conn *websocket.Conn) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.delivered {
		return false
	}
	a.delivered = true
	a.tunnel = conn
	a.readyOnce.Do(func() { close(a.ready) })
	return true
}

// cancel closes the attachment and any socket it already owns. Safe repeatedly.
func (a *attachment) cancel() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return
	}
	a.closed = true
	if a.tunnel != nil {
		a.tunnel.Close()
	}
	a.readyOnce.Do(func() { close(a.ready) })
}

// take returns the delivered tunnel, or nil if the attachment was cancelled.
func (a *attachment) take() *websocket.Conn {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	return a.tunnel
}

// New creates a coordinator.
func New(opts Options) *Coordinator {
	if opts.Logger == nil {
		opts.Logger = log.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	// Only fall back to the admin token when durable identity is disabled. With
	// a store, the admin token must never authenticate a worker.
	if opts.WorkerToken == "" && opts.Store == nil {
		opts.WorkerToken = opts.AdminToken
	}
	return &Coordinator{
		opts:           opts,
		store:          opts.Store,
		log:            opts.Logger,
		now:            opts.Now,
		nodes:          make(map[string]*Node),
		leases:         make(map[string]*Lease),
		attachments:    make(map[string]*attachment),
		revokedDevices: make(map[string]struct{}),
		relays:         make(map[*Node]map[uint64]func()),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  32 * 1024,
			WriteBufferSize: 32 * 1024,
			// Authentication is by bearer token, not Origin. CDP clients and
			// workers are not browsers.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
}

func (c *Coordinator) logf(format string, args ...any) {
	c.log.Printf(format, args...)
}

// ---------------------------------------------------------------- node send

func (n *Node) sendMessage(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	select {
	case n.send <- b:
		return nil
	case <-n.closed:
		return errors.New("node disconnected")
	case <-time.After(5 * time.Second):
		return errors.New("node send buffer full")
	}
}

func (n *Node) close() {
	n.once.Do(func() {
		close(n.closed)
		if n.conn != nil {
			n.conn.Close()
		}
	})
}

// ---------------------------------------------------------------- leases

// publishAttachment registers the active attachment unless a pause won the
// race. The caller must have already recorded the controller on the attachment,
// so a pause that observes the attachment can drop it.
func (l *Lease) publishAttachment(a *attachment) bool {
	l.attachMu.Lock()
	defer l.attachMu.Unlock()
	if l.paused {
		return false
	}
	l.attachment = a
	return true
}

func (l *Lease) finish(state, errMsg string) {
	l.doneOnce.Do(func() {
		l.State = state
		l.Err = errMsg
		close(l.done)
	})
}

func (c *Coordinator) newLeaseToken() string {
	// 256 bits of entropy, URL-safe.
	return randomToken(32)
}

// leaseRequest is the caller-facing request to acquire a browser.
type leaseRequest struct {
	Node       string `json:"node,omitempty"`
	Browser    string `json:"browser,omitempty"`
	Profile    string `json:"profile,omitempty"`
	Headful    bool   `json:"headful,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
	StartURL   string `json:"start_url,omitempty"`
}

type leaseResponse struct {
	LeaseID  string `json:"lease_id"`
	State    string `json:"state"`
	Node     string `json:"node"`
	Browser  string `json:"browser,omitempty"`
	Profile  string `json:"profile,omitempty"`
	Headful  bool   `json:"headful"`
	Paused   bool   `json:"paused"`
	Endpoint string `json:"endpoint"`
	Expires  string `json:"expires"`
	Error    string `json:"error,omitempty"`
}

const (
	defaultTTL = 30 * time.Minute
	maxTTL     = 24 * time.Hour
	readyWait  = 45 * time.Second
	tunnelWait = 30 * time.Second
)

// selectNode picks an eligible node for the request. It is a hard filter
// followed by least-loaded selection. Callers must hold c.mu.
func (c *Coordinator) selectNode(req leaseRequest) (*Node, error) {
	var candidates []*Node
	for _, n := range c.nodes {
		if !n.online {
			continue
		}
		if req.Node != "" && n.Name != req.Node && n.ID != req.Node {
			continue
		}
		if req.Browser != "" && !hasBrowser(n.Browsers, req.Browser) {
			continue
		}
		if req.Profile != "" && !hasString(n.Profiles, req.Profile) {
			continue
		}
		if req.Headful && !n.Headful {
			continue
		}
		if n.MaxSlots > 0 && n.active >= n.MaxSlots {
			continue
		}
		candidates = append(candidates, n)
	}
	if len(candidates) == 0 {
		return nil, errNoEligibleNode(req)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].active != candidates[j].active {
			return candidates[i].active < candidates[j].active
		}
		return candidates[i].Name < candidates[j].Name
	})
	return candidates[0], nil
}

func errNoEligibleNode(req leaseRequest) error {
	switch {
	case req.Profile != "":
		return fmt.Errorf("no online node offers profile %q", req.Profile)
	case req.Node != "":
		return fmt.Errorf("node %q is not online or does not match the request", req.Node)
	case req.Headful:
		return errors.New("no online node can run a headful browser")
	case req.Browser != "":
		return fmt.Errorf("no online node offers browser %q", req.Browser)
	default:
		return errors.New("no online worker is available")
	}
}

func hasBrowser(list []protocol.Browser, name string) bool {
	for _, b := range list {
		if b.Name == name {
			return true
		}
	}
	return false
}

func hasString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// acquire selects a node, reserves capacity, and asks it to launch a browser.
// It returns once the worker reports ready, fails, or the wait elapses.
func (c *Coordinator) acquire(req leaseRequest, ctxDone <-chan struct{}) (*Lease, error) {
	ttl := defaultTTL
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	if ttl > maxTTL {
		return nil, fmt.Errorf("ttl_seconds exceeds maximum of %d", int(maxTTL.Seconds()))
	}

	c.mu.Lock()
	node, err := c.selectNode(req)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	lease := &Lease{
		ID:       "lse_" + randomToken(9),
		NodeID:   node.ID,
		node:     node,
		Browser:  req.Browser,
		Profile:  req.Profile,
		Headful:  req.Headful,
		Token:    c.newLeaseToken(),
		State:    StateStarting,
		Created:  c.now(),
		Expires:  c.now().Add(ttl),
		reserved: true,
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	node.active++
	c.leases[lease.ID] = lease
	c.mu.Unlock()

	if err := node.sendMessage(protocol.Launch{
		Type:       protocol.TypeLaunch,
		LeaseID:    lease.ID,
		Browser:    req.Browser,
		Headful:    req.Headful,
		Profile:    req.Profile,
		TTLSeconds: int(ttl.Seconds()),
		StartURL:   req.StartURL,
	}); err != nil {
		c.finishLease(lease.ID, StateFailed, "node disconnected before launch")
		return lease, fmt.Errorf("worker unavailable: %w", err)
	}

	lease.expireTmr = time.AfterFunc(ttl, func() { c.expireLease(lease.ID) })

	select {
	case <-lease.done:
	case <-time.After(readyWait):
		go c.releaseLease(lease.ID, StateFailed, "timed out waiting for browser")
	case <-ctxDone:
		go c.releaseLease(lease.ID, StateClosed, "caller disconnected")
	}
	return lease, nil
}

func (c *Coordinator) expireLease(id string) {
	c.logf("lease %s expired", id)
	c.releaseLease(id, StateExpired, "")
}

// releaseLease stops a lease and frees its slot. Safe to call repeatedly.
func (c *Coordinator) releaseLease(id, state, errMsg string) {
	c.mu.Lock()
	lease, ok := c.leases[id]
	if !ok {
		c.mu.Unlock()
		return
	}
	node := lease.node
	wasReserved := lease.reserved
	if state == "" {
		state = StateClosed
	}
	if lease.State == StateStarting || lease.State == StateReady {
		lease.State = StateStopping
	}
	lease.reserved = false
	c.mu.Unlock()

	if lease.expireTmr != nil {
		lease.expireTmr.Stop()
	}
	if node != nil && node.online {
		_ = node.sendMessage(protocol.Release{Type: protocol.TypeRelease, LeaseID: id})
	} else {
		c.signalStopped(lease)
	}
	// The worker enforces its own TTL, so we free capacity even if the release
	// acknowledgement never arrives.
	if wasReserved {
		c.mu.Lock()
		if n := lease.node; n != nil && n.active > 0 {
			n.active--
		}
		c.mu.Unlock()
	}
	lease.doneOnce.Do(func() {
		lease.State = state
		lease.Err = errMsg
		close(lease.done)
	})
}

// signalStopped records that a lease's browser is known to be gone.
func (c *Coordinator) signalStopped(l *Lease) {
	l.stopOnce.Do(func() { close(l.stopped) })
}

// finishLease is used when a worker reports a terminal outcome.
func (c *Coordinator) finishLease(id, state, errMsg string) {
	c.mu.Lock()
	lease, ok := c.leases[id]
	if !ok {
		c.mu.Unlock()
		return
	}
	wasReserved := lease.reserved
	lease.reserved = false
	c.mu.Unlock()

	if wasReserved {
		c.mu.Lock()
		if n := lease.node; n != nil && n.active > 0 {
			n.active--
		}
		c.mu.Unlock()
	}
	lease.finish(state, errMsg)
}

// ---------------------------------------------------------------- attachments

func (c *Coordinator) openAttachment(lease *Lease) (*attachment, error) {
	c.mu.Lock()
	node := lease.node
	if node == nil || !node.online || node.dead {
		c.mu.Unlock()
		return nil, errors.New("worker is offline")
	}
	a := &attachment{
		id:          "att_" + randomToken(9),
		leaseID:     lease.ID,
		node:        node,
		tunnelToken: randomToken(32),
		created:     c.now(),
		ready:       make(chan struct{}),
	}
	c.attachments[a.id] = a
	c.mu.Unlock()

	if err := node.sendMessage(protocol.OpenTunnel{
		Type:         protocol.TypeOpenTunnel,
		LeaseID:      lease.ID,
		AttachmentID: a.id,
		TunnelToken:  a.tunnelToken,
	}); err != nil {
		c.removeAttachment(a)
		return nil, fmt.Errorf("worker unavailable: %w", err)
	}
	return a, nil
}

func (c *Coordinator) removeAttachment(a *attachment) {
	a.cancel()
	c.mu.Lock()
	delete(c.attachments, a.id)
	c.mu.Unlock()
}

// addRelay registers a cancellable relay for an exact control session. It
// refuses (ok=false) if the session is already dead, so a relay that lost the
// race against revocation is never admitted. The returned function removes the
// registration; the registered function closes one end so relay.Pipe unwinds.
func (c *Coordinator) addRelay(n *Node, closeFn func()) (remove func(), ok bool) {
	c.mu.Lock()
	if n == nil || n.dead {
		c.mu.Unlock()
		return nil, false
	}
	c.relaySeq++
	id := c.relaySeq
	if c.relays[n] == nil {
		c.relays[n] = make(map[uint64]func())
	}
	c.relays[n][id] = closeFn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		if m := c.relays[n]; m != nil {
			delete(m, id)
			if len(m) == 0 {
				delete(c.relays, n)
			}
		}
		c.mu.Unlock()
	}, true
}

// markNodeDead records that a session is finished and returns its relays.
// Callers must set dead and collect relays under the coordinator lock so that
// addRelay cannot admit a relay after cleanup.
func (c *Coordinator) takeRelaysLocked(n *Node) []func() {
	n.dead = true
	m := c.relays[n]
	delete(c.relays, n)
	out := make([]func(), 0, len(m))
	for _, fn := range m {
		out = append(out, fn)
	}
	return out
}

func closeAll(fns []func()) {
	for _, fn := range fns {
		fn()
	}
}

func (c *Coordinator) takeAttachment(id, token string) (*attachment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.attachments[id]
	if !ok {
		return nil, errors.New("unknown attachment")
	}
	if a.tunnelToken != token {
		return nil, errors.New("invalid tunnel token")
	}
	return a, nil
}

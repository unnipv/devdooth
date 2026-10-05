package coordinator

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/unnipv/devdooth/internal/protocol"
	"github.com/unnipv/devdooth/internal/relay"
	"github.com/unnipv/devdooth/internal/webui"
)

const (
	controlReadTimeout  = 90 * time.Second
	controlWriteTimeout = 30 * time.Second
	controlSendBuffer   = 64
)

// Handler returns the coordinator's HTTP handler.
func (c *Coordinator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", c.handleHealth)
	mux.HandleFunc("GET /v1/nodes", c.authAdmin(c.handleNodes))
	mux.HandleFunc("POST /v1/leases", c.authAdmin(c.handleAcquire))
	mux.HandleFunc("GET /v1/leases/{id}", c.authAdmin(c.handleGetLease))
	mux.HandleFunc("DELETE /v1/leases/{id}", c.authAdmin(c.handleDeleteLease))
	mux.HandleFunc("POST /v1/leases/{id}/pause", c.authAdmin(c.handlePauseLease))
	mux.HandleFunc("POST /v1/leases/{id}/resume", c.authAdmin(c.handleResumeLease))
	mux.HandleFunc("POST /v1/enroll-tokens", c.authAdmin(c.handleCreateEnrollToken))
	mux.HandleFunc("GET /v1/devices", c.authAdmin(c.handleListDevices))
	mux.HandleFunc("DELETE /v1/devices/{id}", c.authAdmin(c.handleRevokeDevice))
	mux.HandleFunc("POST /v1/enroll", c.handleEnroll)
	mux.HandleFunc("GET /v1/lease/{id}/cdp", c.handleClientAttach)
	mux.HandleFunc("GET /v1/worker/connect", c.handleWorkerConnect)
	mux.HandleFunc("GET /v1/tunnel/{id}", c.handleWorkerTunnel)
	mux.Handle("GET /", webui.Handler())
	return mux
}

// ---------------------------------------------------------------- middleware

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return r.URL.Query().Get("token")
}

func (c *Coordinator) authAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c.opts.AdminToken == "" || subtleEqual(bearer(r), c.opts.AdminToken) {
			next(w, r)
			return
		}
		httpError(w, http.StatusUnauthorized, "invalid or missing admin token")
	}
}

func (c *Coordinator) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// ---------------------------------------------------------------- nodes

type nodeView struct {
	ID       string             `json:"id"`
	Name     string             `json:"name"`
	OS       string             `json:"os"`
	Arch     string             `json:"arch"`
	Headful  bool               `json:"headful"`
	Browsers []protocol.Browser `json:"browsers"`
	Profiles []string           `json:"profiles"`
	Slots    int                `json:"slots"`
	Active   int                `json:"active"`
	Online   bool               `json:"online"`
}

func (c *Coordinator) handleNodes(w http.ResponseWriter, _ *http.Request) {
	c.mu.Lock()
	views := make([]nodeView, 0, len(c.nodes))
	for _, n := range c.nodes {
		views = append(views, nodeView{
			ID: n.ID, Name: n.Name, OS: n.OS, Arch: n.Arch, Headful: n.Headful,
			Browsers: n.Browsers, Profiles: n.Profiles, Slots: n.MaxSlots,
			Active: n.active, Online: n.online,
		})
	}
	c.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"nodes": views})
}

// ---------------------------------------------------------------- leases

func (c *Coordinator) handleAcquire(w http.ResponseWriter, r *http.Request) {
	var req leaseRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	lease, err := c.acquire(req, r.Context().Done())
	if lease == nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	if lease.State == StateFailed {
		writeJSON(w, http.StatusBadGateway, c.leaseResponse(lease, r))
		return
	}
	writeJSON(w, http.StatusOK, c.leaseResponse(lease, r))
}

func (c *Coordinator) handleGetLease(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	lease, ok := c.leases[r.PathValue("id")]
	c.mu.Unlock()
	if !ok {
		httpError(w, http.StatusNotFound, "no such lease")
		return
	}
	writeJSON(w, http.StatusOK, c.leaseResponse(lease, r))
}

func (c *Coordinator) handleDeleteLease(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c.mu.Lock()
	lease, ok := c.leases[id]
	c.mu.Unlock()
	if !ok {
		httpError(w, http.StatusNotFound, "no such lease")
		return
	}
	c.releaseLease(id, StateClosed, "")
	// Wait for the worker to confirm the browser stopped so callers can rely on
	// teardown having happened. Bounded: the worker may have vanished.
	select {
	case <-lease.stopped:
	case <-time.After(15 * time.Second):
	}
	writeJSON(w, http.StatusOK, map[string]string{"lease_id": id, "state": StateClosed})
}

// handlePauseLease ends the current controller's attachment and marks the lease
// paused, so a human can use a headful browser on the worker. It does not stop
// the browser. CDP commands already in flight are not undone, and the agent's
// client is disconnected; it reattaches after Resume.
func (c *Coordinator) handlePauseLease(w http.ResponseWriter, r *http.Request) {
	lease, ok := c.lookupLease(w, r)
	if !ok {
		return
	}
	lease.attachMu.Lock()
	lease.paused = true
	a := lease.attachment
	lease.attachMu.Unlock()
	if a != nil {
		a.dropController()
	}
	c.logf("lease %s paused for human take-over", lease.ID)
	writeJSON(w, http.StatusOK, map[string]any{"lease_id": lease.ID, "paused": true})
}

// handleResumeLease allows a controller to attach again.
func (c *Coordinator) handleResumeLease(w http.ResponseWriter, r *http.Request) {
	lease, ok := c.lookupLease(w, r)
	if !ok {
		return
	}
	lease.attachMu.Lock()
	lease.paused = false
	lease.attachMu.Unlock()
	c.logf("lease %s resumed", lease.ID)
	writeJSON(w, http.StatusOK, map[string]any{"lease_id": lease.ID, "paused": false})
}

func (c *Coordinator) lookupLease(w http.ResponseWriter, r *http.Request) (*Lease, bool) {
	c.mu.Lock()
	lease, ok := c.leases[r.PathValue("id")]
	c.mu.Unlock()
	if !ok {
		httpError(w, http.StatusNotFound, "no such lease")
		return nil, false
	}
	return lease, true
}

func (c *Coordinator) leaseResponse(l *Lease, r *http.Request) leaseResponse {
	c.mu.Lock()
	nodeName := ""
	if n := c.nodes[l.NodeID]; n != nil {
		nodeName = n.Name
	}
	c.mu.Unlock()
	l.attachMu.Lock()
	paused := l.paused
	l.attachMu.Unlock()
	return leaseResponse{
		LeaseID:  l.ID,
		State:    l.State,
		Node:     nodeName,
		Browser:  l.Browser,
		Profile:  l.Profile,
		Headful:  l.Headful,
		Paused:   paused,
		Endpoint: c.publicBase(r) + "/v1/lease/" + l.ID + "/cdp?token=" + l.Token,
		Expires:  l.Expires.UTC().Format(time.RFC3339),
		Error:    l.Err,
	}
}

func (c *Coordinator) publicBase(r *http.Request) string {
	if c.opts.PublicURL != "" {
		return wsScheme(c.opts.PublicURL)
	}
	scheme := "ws"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "wss"
	}
	return scheme + "://" + r.Host
}

// wsScheme normalises a public base URL to a WebSocket scheme. Callers may set
// --public-url to an http(s) URL behind a reverse proxy; lease endpoints must
// still be ws(s) so CDP clients can connect.
func wsScheme(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		return "ws://" + strings.TrimPrefix(base, "http://")
	default:
		return base
	}
}

// ---------------------------------------------------------------- client attach

func (c *Coordinator) handleClientAttach(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	c.mu.Lock()
	lease, ok := c.leases[id]
	c.mu.Unlock()
	if !ok {
		httpError(w, http.StatusNotFound, "no such lease")
		return
	}

	tok := bearer(r)
	if tok == "" || !(subtleEqual(tok, lease.Token) || subtleEqual(tok, c.opts.AdminToken)) {
		httpError(w, http.StatusUnauthorized, "invalid session credential")
		return
	}
	if lease.State != StateReady {
		httpError(w, http.StatusConflict, "lease is "+lease.State+", not ready")
		return
	}

	lease.attachMu.Lock()
	if lease.paused {
		lease.attachMu.Unlock()
		httpError(w, http.StatusConflict, "lease is paused for human take-over; resume it first")
		return
	}
	if lease.attached {
		lease.attachMu.Unlock()
		httpError(w, http.StatusConflict, "lease already has an active controller")
		return
	}
	lease.attached = true
	lease.attachMu.Unlock()
	defer func() {
		lease.attachMu.Lock()
		lease.attached = false
		lease.attachment = nil
		lease.attachMu.Unlock()
	}()

	a, err := c.openAttachment(lease)
	if err != nil {
		httpError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer c.removeAttachment(a)

	client, err := c.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer client.Close()

	// Record the controller before publishing the attachment, then publish and
	// re-check paused under the same lock. A pause racing this setup either sees
	// the attachment (and drops the controller) or wins first (and we refuse to
	// start relaying).
	a.setClient(client)
	if !lease.publishAttachment(a) {
		return
	}

	select {
	case <-a.ready:
		tunnel := a.take()
		if tunnel == nil {
			return // cancelled while we waited
		}
		defer tunnel.Close()
		removeRelay, ok := c.addRelay(lease.node, func() { client.Close() })
		if !ok {
			return // session died before we could admit the relay
		}
		defer removeRelay()
		_ = relay.Pipe(client, tunnel)
	case <-time.After(tunnelWait):
		client.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "tunnel timeout"),
			time.Now().Add(5*time.Second))
	case <-lease.node.closed:
		// The worker is gone (disconnected or revoked).
	case <-r.Context().Done():
	}
}

// ---------------------------------------------------------------- worker control

func (c *Coordinator) handleWorkerConnect(w http.ResponseWriter, r *http.Request) {
	device, ok := c.authenticateWorker(r)
	if !ok {
		httpError(w, http.StatusUnauthorized, "invalid worker or device token")
		return
	}
	conn, err := c.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	conn.SetReadLimit(1 << 20)
	_ = conn.SetReadDeadline(time.Now().Add(controlReadTimeout))
	conn.SetPingHandler(func(data string) error {
		_ = conn.SetReadDeadline(time.Now().Add(controlReadTimeout))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(10*time.Second))
	})

	var hello protocol.Hello
	if err := conn.ReadJSON(&hello); err != nil || hello.Type != protocol.TypeHello {
		conn.Close()
		return
	}

	node := c.registerNode(&hello, conn, device)
	if node == nil {
		return
	}
	c.logf("worker %q connected (%s/%s, %d browser(s), %d slot(s))",
		node.Name, node.OS, node.Arch, len(node.Browsers), node.MaxSlots)
	if device != nil {
		_ = c.store.TouchDevice(device.ID)
	}

	go c.writer(node, conn)
	defer c.disconnectNode(node)

	for {
		_ = conn.SetReadDeadline(time.Now().Add(controlReadTimeout))
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		c.mu.Lock()
		node.lastSeen = c.now()
		c.mu.Unlock()
		if device != nil {
			_ = c.store.TouchDevice(device.ID)
		}
		c.handleWorkerMessage(node, data)
	}
}

func (c *Coordinator) registerNode(h *protocol.Hello, conn *websocket.Conn, device *Device) *Node {
	// An enrolled device's name is fixed at enrollment; the worker cannot
	// rename itself on reconnect or impersonate another node.
	id, name := h.Name, h.Name
	deviceID := ""
	if device != nil {
		id, name, deviceID = device.Name, device.Name, device.ID
	}

	c.mu.Lock()
	if device != nil {
		if _, revoked := c.revokedDevices[device.ID]; revoked {
			c.mu.Unlock()
			c.logf("rejecting registration for revoked device %q", device.Name)
			conn.Close()
			return nil
		}
	}
	var replaced *Node
	var replacedRelays []func()
	if existing, ok := c.nodes[id]; ok && existing.online {
		replaced = existing
		replacedRelays = c.takeRelaysLocked(existing)
	}
	maxSlots := h.MaxSlots
	if maxSlots <= 0 {
		maxSlots = 1
	}
	node := &Node{
		ID: id, Name: name, DeviceID: deviceID,
		OS: h.OS, Arch: h.Arch, Headful: h.Headful,
		Browsers: h.Browsers, Profiles: h.Profiles, MaxSlots: maxSlots,
		Generation: h.Generation, conn: conn,
		send: make(chan []byte, controlSendBuffer), closed: make(chan struct{}),
		online: true, lastSeen: c.now(),
	}
	c.nodes[node.ID] = node
	c.mu.Unlock()

	if replaced != nil {
		c.logf("worker %q reconnected; invalidating previous generation", node.Name)
		closeAll(replacedRelays)
		replaced.close()
		c.failNodeLeases(replaced, "worker reconnected")
	}
	return node
}

func (c *Coordinator) writer(n *Node, conn *websocket.Conn) {
	for {
		select {
		case msg := <-n.send:
			_ = conn.SetWriteDeadline(time.Now().Add(controlWriteTimeout))
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				n.close()
				return
			}
		case <-n.closed:
			return
		}
	}
}

func (c *Coordinator) disconnectNode(n *Node) {
	c.mu.Lock()
	if current, ok := c.nodes[n.ID]; ok && current == n {
		n.online = false
	}
	relays := c.takeRelaysLocked(n)
	c.mu.Unlock()
	closeAll(relays)
	n.close()
	c.logf("worker %q disconnected", n.Name)
	c.failNodeLeases(n, "worker disconnected")
}

func (c *Coordinator) failNodeLeases(n *Node, reason string) {
	c.mu.Lock()
	var ids []string
	for id, l := range c.leases {
		if l.node == n && (l.State == StateStarting || l.State == StateReady || l.State == StateStopping) {
			ids = append(ids, id)
		}
	}
	c.mu.Unlock()
	for _, id := range ids {
		c.mu.Lock()
		lease := c.leases[id]
		c.mu.Unlock()
		if lease != nil {
			c.signalStopped(lease)
		}
		c.releaseLease(id, StateFailed, reason)
	}
}

func (c *Coordinator) handleWorkerMessage(n *Node, data []byte) {
	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		c.logf("worker %q sent malformed message", n.Name)
		return
	}
	switch env.Type {
	case protocol.TypeReady:
		var m protocol.Ready
		if json.Unmarshal(data, &m) == nil {
			c.markReady(n, m.LeaseID)
		}
	case protocol.TypeLaunchFail:
		var m protocol.LaunchFail
		if json.Unmarshal(data, &m) == nil {
			c.logf("worker %q failed to launch lease %s: %s", n.Name, m.LeaseID, m.Error)
			c.failLeaseOwned(n, m.LeaseID, m.Error)
		}
	case protocol.TypeReleased:
		var m protocol.Released
		if json.Unmarshal(data, &m) == nil {
			c.markReleasedOwned(n, m.LeaseID)
		}
	case protocol.TypePong:
		// liveness only
	}
}

func (c *Coordinator) markReady(n *Node, leaseID string) {
	c.mu.Lock()
	lease, ok := c.leases[leaseID]
	if !ok || lease.node != n {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	if lease.State == StateStarting {
		lease.State = StateReady
		lease.doneOnce.Do(func() { close(lease.done) })
	}
}

// ownedLease returns the lease only if it belongs to this control session. A
// worker must not be able to affect a lease it does not own.
func (c *Coordinator) ownedLease(n *Node, id string) *Lease {
	c.mu.Lock()
	defer c.mu.Unlock()
	lease := c.leases[id]
	if lease == nil || lease.node != n {
		return nil
	}
	return lease
}

func (c *Coordinator) failLeaseOwned(n *Node, id, msg string) {
	if c.ownedLease(n, id) == nil {
		c.logf("ignoring launch_failed for lease %s from a session that does not own it", id)
		return
	}
	c.finishLease(id, StateFailed, msg)
}

func (c *Coordinator) markReleasedOwned(n *Node, id string) {
	lease := c.ownedLease(n, id)
	if lease == nil {
		return
	}
	c.signalStopped(lease)
	c.finishLease(id, StateClosed, "")
}

// ---------------------------------------------------------------- worker tunnel

func (c *Coordinator) handleWorkerTunnel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, err := c.takeAttachment(id, bearer(r))
	if err != nil {
		httpError(w, http.StatusUnauthorized, "invalid attachment or token")
		return
	}
	conn, err := c.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	if !a.deliver(conn) {
		conn.Close() // the client already gave up
	}
}

// ---------------------------------------------------------------- helpers

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// subtleEqual compares two strings without leaking length or content timing.
func subtleEqual(a, b string) bool {
	if a == "" || b == "" || len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

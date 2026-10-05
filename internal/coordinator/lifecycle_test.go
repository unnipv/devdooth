package coordinator_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/unnipv/devdooth/internal/coordinator"
	"github.com/unnipv/devdooth/internal/protocol"
)

// fakeWorker is a stand-in for a real worker. It speaks the control protocol
// and, when asked to open a tunnel, echoes every message back. That exercises
// the full coordinator relay without launching a browser.
type fakeWorker struct {
	t      *testing.T
	conn   *websocket.Conn
	wsBase string
	hello  protocol.Hello
}

func newFakeWorker(t *testing.T, serverURL string) *fakeWorker {
	t.Helper()
	wsBase := "ws" + strings.TrimPrefix(serverURL, "http")
	hdr := http.Header{"Authorization": {"Bearer worker-token"}}
	conn, _, err := websocket.DefaultDialer.Dial(wsBase+"/v1/worker/connect", hdr)
	if err != nil {
		t.Fatalf("worker dial: %v", err)
	}
	w := &fakeWorker{t: t, conn: conn, wsBase: wsBase, hello: protocol.Hello{
		Type: protocol.TypeHello, Name: "fake", OS: "test", Arch: "test",
		Headful: true, MaxSlots: 2,
		Browsers:   []protocol.Browser{{Name: "chrome", Path: "/fake/chrome"}},
		Profiles:   []string{"shopping"},
		Generation: "gen-1",
	}}
	if err := conn.WriteJSON(w.hello); err != nil {
		t.Fatalf("worker hello: %v", err)
	}
	go w.loop()
	return w
}

func (w *fakeWorker) loop() {
	for {
		_, data, err := w.conn.ReadMessage()
		if err != nil {
			return
		}
		var env protocol.Envelope
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		switch env.Type {
		case protocol.TypeLaunch:
			var m protocol.Launch
			_ = json.Unmarshal(data, &m)
			_ = w.conn.WriteJSON(protocol.Ready{Type: protocol.TypeReady, LeaseID: m.LeaseID})
		case protocol.TypeRelease:
			var m protocol.Release
			_ = json.Unmarshal(data, &m)
			_ = w.conn.WriteJSON(protocol.Released{Type: protocol.TypeReleased, LeaseID: m.LeaseID})
		case protocol.TypeOpenTunnel:
			var m protocol.OpenTunnel
			_ = json.Unmarshal(data, &m)
			go w.tunnel(m)
		}
	}
}

func (w *fakeWorker) tunnel(m protocol.OpenTunnel) {
	hdr := http.Header{"Authorization": {"Bearer " + m.TunnelToken}}
	conn, _, err := websocket.DefaultDialer.Dial(w.wsBase+"/v1/tunnel/"+m.AttachmentID, hdr)
	if err != nil {
		return
	}
	defer conn.Close()
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if err := conn.WriteMessage(mt, data); err != nil {
			return
		}
	}
}

func newTestServer(t *testing.T) (*httptest.Server, *fakeWorker) {
	t.Helper()
	c := coordinator.New(coordinator.Options{AdminToken: "admin-token", WorkerToken: "worker-token"})
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)
	fw := newFakeWorker(t, srv.URL)
	t.Cleanup(func() { fw.conn.Close() })
	// Give the coordinator a moment to register the node.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if nodeCount(t, srv.URL) > 0 {
			return srv, fw
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worker never registered")
	return nil, nil
}

func nodeCount(t *testing.T, base string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var out struct {
		Nodes []json.RawMessage `json:"nodes"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return len(out.Nodes)
}

func TestLeaseLifecycleAndRelay(t *testing.T) {
	srv, _ := newTestServer(t)
	base := srv.URL

	// Acquire.
	body := strings.NewReader(`{"profile":"shopping","headful":true,"ttl_seconds":60}`)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/leases", body)
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("acquire status %d", resp.StatusCode)
	}
	var lease struct {
		LeaseID  string `json:"lease_id"`
		State    string `json:"state"`
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&lease); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if lease.State != "ready" || lease.LeaseID == "" || lease.Endpoint == "" {
		t.Fatalf("unexpected lease: %+v", lease)
	}

	// Attach and relay a message through the full path.
	conn, _, err := websocket.DefaultDialer.Dial(lease.Endpoint, nil)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteMessage(websocket.TextMessage, []byte("ping-through-relay")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(msg) != "ping-through-relay" {
		t.Fatalf("relay corrupted message: %q", msg)
	}

	// A second controller must be rejected.
	if _, _, err := websocket.DefaultDialer.Dial(lease.Endpoint, nil); err == nil {
		t.Fatal("expected second controller to be rejected")
	}

	// Invalid credential must be rejected before upgrade.
	badEndpoint := strings.Replace(lease.Endpoint, "token="+leaseToken(lease.Endpoint), "token=wrong", 1)
	if _, _, err := websocket.DefaultDialer.Dial(badEndpoint, nil); err == nil {
		t.Fatal("expected invalid credential to be rejected")
	}

	// Release.
	req, _ = http.NewRequest(http.MethodDelete, base+"/v1/leases/"+lease.LeaseID, nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("release status %d", resp2.StatusCode)
	}
}

func leaseToken(endpoint string) string {
	i := strings.Index(endpoint, "token=")
	if i < 0 {
		return ""
	}
	return endpoint[i+len("token="):]
}

func TestAcquireRequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Post(srv.URL+"/v1/leases", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestAcquireFailsWithNoEligibleNode(t *testing.T) {
	c := coordinator.New(coordinator.Options{AdminToken: "admin-token", WorkerToken: "worker-token"})
	srv := httptest.NewServer(c.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/leases", strings.NewReader(`{"profile":"missing"}`))
	req.Header.Set("Authorization", "Bearer admin-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if !strings.Contains(out["error"], "missing") {
		t.Fatalf("expected error to name the profile, got %q", out["error"])
	}
}

package coordinator_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/unnipv/devdooth/internal/protocol"
)

// A device that authenticates, then waits, must not be able to register after
// it has been revoked.
func TestRevokedDeviceCannotRegisterAfterUpgrade(t *testing.T) {
	srv, _ := newEnrollServer(t)
	base := srv.URL

	_, body := adminJSON(t, http.MethodPost, base+"/v1/enroll-tokens", `{}`)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(body, &tok)

	_, body = adminJSON(t, http.MethodPost, base+"/v1/enroll", `{"token":"`+tok.Token+`","name":"macbook"}`)
	var dev struct {
		DeviceID    string `json:"device_id"`
		DeviceToken string `json:"device_token"`
	}
	_ = json.Unmarshal(body, &dev)

	// Open the control socket but do not send a hello yet.
	wsBase := "ws" + strings.TrimPrefix(base, "http")
	hdr := http.Header{"Authorization": {"Bearer " + dev.DeviceToken}}
	conn, _, err := websocket.DefaultDialer.Dial(wsBase+"/v1/worker/connect", hdr)
	if err != nil {
		t.Fatalf("worker dial: %v", err)
	}
	defer conn.Close()

	// Revoke while the connection is parked before hello.
	if code, _ := adminJSON(t, http.MethodDelete, base+"/v1/devices/"+dev.DeviceID, ""); code != http.StatusOK {
		t.Fatalf("revoke failed: %d", code)
	}

	// Now send the hello. The coordinator must refuse to register it.
	_ = conn.WriteJSON(protocol.Hello{Type: protocol.TypeHello, Name: "macbook", MaxSlots: 1, Generation: "g"})
	time.Sleep(150 * time.Millisecond)

	code, body := adminJSON(t, http.MethodGet, base+"/v1/nodes", "")
	if code != http.StatusOK {
		t.Fatalf("nodes: %d", code)
	}
	if strings.Contains(string(body), `"macbook"`) {
		t.Fatalf("revoked device registered as a node: %s", body)
	}
}

// The admin token must never authenticate a worker when durable identity is on.
func TestAdminTokenNotAcceptedAsWorkerInDurableMode(t *testing.T) {
	srv, _ := newEnrollServer(t)
	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http")
	hdr := http.Header{"Authorization": {"Bearer admin-token"}}
	if _, _, err := websocket.DefaultDialer.Dial(wsBase+"/v1/worker/connect", hdr); err == nil {
		t.Fatal("admin token must not open a worker control channel")
	}
}

func TestEnrollmentWithoutAdminHeaderSucceeds(t *testing.T) {
	srv, _ := newEnrollServer(t)
	_, body := adminJSON(t, http.MethodPost, srv.URL+"/v1/enroll-tokens", `{}`)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(body, &tok)

	// Deliberately no Authorization header: the enrollment token is the credential.
	resp, err := http.Post(srv.URL+"/v1/enroll", "application/json",
		bytes.NewBufferString(`{"token":"`+tok.Token+`","name":"pi"}`))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 without an admin header, got %d", resp.StatusCode)
	}
}

func TestMalformedEnrollmentTokenRequestRejected(t *testing.T) {
	srv, _ := newEnrollServer(t)

	for name, body := range map[string]string{
		"not json":     `{`,
		"wrong type":   `{"ttl_seconds":"soon"}`,
		"negative ttl": `{"ttl_seconds":-1}`,
	} {
		t.Run(name, func(t *testing.T) {
			code, _ := adminJSON(t, http.MethodPost, srv.URL+"/v1/enroll-tokens", body)
			if code != http.StatusBadRequest {
				t.Fatalf("expected 400 for %s, got %d", name, code)
			}
		})
	}

	// An overflowing TTL must not silently mint a token.
	if code, _ := adminJSON(t, http.MethodPost, srv.URL+"/v1/enroll-tokens", `{"ttl_seconds":999999999999}`); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for huge ttl, got %d", code)
	}
}

func TestDuplicateDeviceNameReturnsConflict(t *testing.T) {
	srv, _ := newEnrollServer(t)
	mint := func() string {
		_, body := adminJSON(t, http.MethodPost, srv.URL+"/v1/enroll-tokens", `{}`)
		var tok struct {
			Token string `json:"token"`
		}
		_ = json.Unmarshal(body, &tok)
		return tok.Token
	}
	if code, _ := adminJSON(t, http.MethodPost, srv.URL+"/v1/enroll", `{"token":"`+mint()+`","name":"macbook"}`); code != http.StatusCreated {
		t.Fatalf("first enroll failed")
	}
	if code, _ := adminJSON(t, http.MethodPost, srv.URL+"/v1/enroll", `{"token":"`+mint()+`","name":"macbook"}`); code != http.StatusConflict {
		t.Fatalf("expected 409 for duplicate name, got %d", code)
	}
}

// Revocation must drop an active relay, not just the control connection.
func TestRevocationDropsActiveRelay(t *testing.T) {
	srv, _ := newEnrollServer(t)

	_, body := adminJSON(t, http.MethodPost, srv.URL+"/v1/enroll-tokens", `{}`)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(body, &tok)
	_, body = adminJSON(t, http.MethodPost, srv.URL+"/v1/enroll", `{"token":"`+tok.Token+`","name":"macbook"}`)
	var dev struct {
		DeviceID    string `json:"device_id"`
		DeviceToken string `json:"device_token"`
	}
	_ = json.Unmarshal(body, &dev)

	fw := &relayWorker{t: t, base: srv.URL, token: dev.DeviceToken}
	fw.connect()
	defer fw.close()

	// Acquire a lease on the fake worker.
	lease := acquireLease(t, srv.URL, `{"node":"macbook","ttl_seconds":60}`)

	conn, _, err := websocket.DefaultDialer.Dial(lease.Endpoint, nil)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer conn.Close()

	// Verify the relay works, then revoke the device under it.
	if err := conn.WriteMessage(websocket.TextMessage, []byte("before-revoke")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, msg, err := conn.ReadMessage(); err != nil || string(msg) != "before-revoke" {
		t.Fatalf("relay not working before revoke: %v %q", err, msg)
	}

	if code, _ := adminJSON(t, http.MethodDelete, srv.URL+"/v1/devices/"+dev.DeviceID, ""); code != http.StatusOK {
		t.Fatalf("revoke failed: %d", code)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("expected the relay to be closed after revocation")
	}
}

// ---------------------------------------------------------------- helpers

type testLease struct {
	LeaseID  string `json:"lease_id"`
	State    string `json:"state"`
	Endpoint string `json:"endpoint"`
}

func acquireLease(t *testing.T, base, body string) testLease {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/leases", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("acquire status %d: %s", resp.StatusCode, buf.String())
	}
	var out testLease
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		t.Fatalf("decode lease: %v", err)
	}
	return out
}

// relayWorker is a fake worker that authenticates with a device token and
// echoes tunnel traffic, so tests can exercise revocation of a live relay.
type relayWorker struct {
	t     *testing.T
	base  string
	token string
	conn  *websocket.Conn
}

func (w *relayWorker) connect() {
	w.t.Helper()
	wsBase := "ws" + strings.TrimPrefix(w.base, "http")
	hdr := http.Header{"Authorization": {"Bearer " + w.token}}
	conn, _, err := websocket.DefaultDialer.Dial(wsBase+"/v1/worker/connect", hdr)
	if err != nil {
		w.t.Fatalf("worker dial: %v", err)
	}
	w.conn = conn
	if err := conn.WriteJSON(protocol.Hello{
		Type: protocol.TypeHello, Name: "macbook", OS: "test", Arch: "test",
		MaxSlots: 2, Browsers: []protocol.Browser{{Name: "chrome", Path: "/fake"}},
		Generation: "gen-1",
	}); err != nil {
		w.t.Fatalf("hello: %v", err)
	}
	go w.loop()
	time.Sleep(50 * time.Millisecond)
}

func (w *relayWorker) loop() {
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

func (w *relayWorker) tunnel(m protocol.OpenTunnel) {
	hdr := http.Header{"Authorization": {"Bearer " + m.TunnelToken}}
	conn, _, err := websocket.DefaultDialer.Dial(w.wsBase()+"/v1/tunnel/"+m.AttachmentID, hdr)
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

func (w *relayWorker) wsBase() string { return "ws" + strings.TrimPrefix(w.base, "http") }

func (w *relayWorker) close() {
	if w.conn != nil {
		w.conn.Close()
	}
}

package coordinator_test

import (
	"bytes"
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

func newEnrollServer(t *testing.T) (*httptest.Server, *coordinator.Store) {
	t.Helper()
	store, err := coordinator.OpenStore("")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	c := coordinator.New(coordinator.Options{AdminToken: "admin-token", WorkerToken: "dev-worker", Store: store})
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)
	return srv, store
}

func adminJSON(t *testing.T, method, url, body string) (int, []byte) {
	t.Helper()
	var rdr *bytes.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(body))
	}
	req, _ := http.NewRequest(method, url, rdr)
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	b := make([]byte, 0, 4096)
	buf := bytes.NewBuffer(b)
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.Bytes()
}

func TestEnrollmentFlowAndRevocation(t *testing.T) {
	srv, _ := newEnrollServer(t)
	base := srv.URL

	// Mint a single-use enrollment token.
	code, body := adminJSON(t, http.MethodPost, base+"/v1/enroll-tokens", `{"label":"mac"}`)
	if code != http.StatusCreated {
		t.Fatalf("create enroll token: %d %s", code, body)
	}
	var tok struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.Token == "" {
		t.Fatalf("bad token response: %s", body)
	}

	// Redeem it.
	code, body = adminJSON(t, http.MethodPost, base+"/v1/enroll", `{"token":"`+tok.Token+`","name":"macbook"}`)
	if code != http.StatusCreated {
		t.Fatalf("enroll: %d %s", code, body)
	}
	var dev struct {
		DeviceID    string `json:"device_id"`
		DeviceToken string `json:"device_token"`
		Name        string `json:"name"`
	}
	if err := json.Unmarshal(body, &dev); err != nil || dev.DeviceToken == "" {
		t.Fatalf("bad enroll response: %s", body)
	}
	if dev.Name != "macbook" {
		t.Fatalf("expected device name macbook, got %q", dev.Name)
	}

	// Replay must fail.
	if code, _ := adminJSON(t, http.MethodPost, base+"/v1/enroll", `{"token":"`+tok.Token+`","name":"other"}`); code != http.StatusUnauthorized {
		t.Fatalf("expected replayed enrollment to be rejected, got %d", code)
	}

	// Connect as the device. The worker tries to rename itself; the coordinator
	// must keep the enrolled name.
	conn := dialWorker(t, base, dev.DeviceToken, "attacker")
	defer conn.Close()

	code, body = adminJSON(t, http.MethodGet, base+"/v1/nodes", "")
	if code != http.StatusOK {
		t.Fatalf("nodes: %d %s", code, body)
	}
	var nodes struct {
		Nodes []struct {
			Name     string `json:"name"`
			DeviceID string `json:"device_id"`
		} `json:"nodes"`
	}
	_ = json.Unmarshal(body, &nodes)
	if len(nodes.Nodes) != 1 || nodes.Nodes[0].Name != "macbook" {
		t.Fatalf("expected node named macbook, got %s", body)
	}

	// Revoke: the live connection must be dropped and the token stop working.
	code, body = adminJSON(t, http.MethodDelete, base+"/v1/devices/"+dev.DeviceID, "")
	if code != http.StatusOK {
		t.Fatalf("revoke: %d %s", code, body)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("expected the revoked device connection to be closed")
	}
	if _, _, err := dialWorkerRaw(base, dev.DeviceToken, "macbook"); err == nil {
		t.Fatal("expected revoked device token to be rejected")
	}
}

func TestStaticWorkerTokenStillWorksWithStore(t *testing.T) {
	srv, _ := newEnrollServer(t)
	conn := dialWorker(t, srv.URL, "dev-worker", "dev-node")
	defer conn.Close()

	code, body := adminJSON(t, http.MethodGet, srv.URL+"/v1/nodes", "")
	if code != http.StatusOK || !strings.Contains(string(body), "dev-node") {
		t.Fatalf("static worker not registered: %d %s", code, body)
	}
}

func TestEnrollWithoutStoreIsUnavailable(t *testing.T) {
	c := coordinator.New(coordinator.Options{AdminToken: "admin-token"})
	srv := httptest.NewServer(c.Handler())
	defer srv.Close()

	code, _ := adminJSON(t, http.MethodPost, srv.URL+"/v1/enroll-tokens", `{}`)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 without a store, got %d", code)
	}
}

func dialWorker(t *testing.T, base, token, helloName string) *websocket.Conn {
	t.Helper()
	conn, _, err := dialWorkerRaw(base, token, helloName)
	if err != nil {
		t.Fatalf("worker dial: %v", err)
	}
	return conn
}

func dialWorkerRaw(base, token, helloName string) (*websocket.Conn, *http.Response, error) {
	wsBase := "ws" + strings.TrimPrefix(base, "http")
	hdr := http.Header{"Authorization": {"Bearer " + token}}
	conn, resp, err := websocket.DefaultDialer.Dial(wsBase+"/v1/worker/connect", hdr)
	if err != nil {
		return nil, resp, err
	}
	hello := protocol.Hello{
		Type: protocol.TypeHello, Name: helloName, OS: "test", Arch: "test",
		MaxSlots: 1, Browsers: []protocol.Browser{{Name: "chrome", Path: "/fake"}},
		Generation: "gen-1",
	}
	if err := conn.WriteJSON(hello); err != nil {
		conn.Close()
		return nil, resp, err
	}
	// Give the coordinator a moment to register the node.
	time.Sleep(50 * time.Millisecond)
	return conn, resp, nil
}

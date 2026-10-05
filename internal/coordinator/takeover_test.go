package coordinator_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Pausing detaches the current controller (so an agent stops) without stopping
// the browser, rejects new controllers, and resume allows reattachment.
func TestPauseDetachesControllerAndBlocksAttach(t *testing.T) {
	srv, _ := newEnrollServer(t)
	device := enrollDevice(t, srv.URL, "macbook")
	w := &relayWorker{t: t, base: srv.URL, token: device.DeviceToken}
	w.connect()
	defer w.close()

	lease := acquireLease(t, srv.URL, `{"node":"macbook","ttl_seconds":120}`)

	conn, _, err := websocket.DefaultDialer.Dial(lease.Endpoint, nil)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	defer conn.Close()
	if err := conn.WriteMessage(websocket.TextMessage, []byte("before-pause")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, msg, err := conn.ReadMessage(); err != nil || string(msg) != "before-pause" {
		t.Fatalf("relay not working before pause: %v %q", err, msg)
	}

	// Pause: the controller connection must be dropped.
	if code, body := adminJSON(t, http.MethodPost, srv.URL+"/v1/leases/"+lease.LeaseID+"/pause", ""); code != http.StatusOK {
		t.Fatalf("pause: %d %s", code, body)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("expected the controller connection to be closed on pause")
	}

	// A new controller is refused while paused.
	if _, _, err := websocket.DefaultDialer.Dial(lease.Endpoint, nil); err == nil {
		t.Fatal("expected attach to be refused while paused")
	}

	// The lease reports paused.
	code, body := adminJSON(t, http.MethodGet, srv.URL+"/v1/leases/"+lease.LeaseID, "")
	if code != http.StatusOK || !strings.Contains(string(body), `"paused":true`) {
		t.Fatalf("expected paused lease, got %d %s", code, body)
	}

	// Resume allows a fresh controller.
	if code, body := adminJSON(t, http.MethodPost, srv.URL+"/v1/leases/"+lease.LeaseID+"/resume", ""); code != http.StatusOK {
		t.Fatalf("resume: %d %s", code, body)
	}
	conn2, _, err := websocket.DefaultDialer.Dial(lease.Endpoint, nil)
	if err != nil {
		t.Fatalf("reattach after resume: %v", err)
	}
	defer conn2.Close()
	if err := conn2.WriteMessage(websocket.TextMessage, []byte("after-resume")); err != nil {
		t.Fatalf("write after resume: %v", err)
	}
	_ = conn2.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, msg, err := conn2.ReadMessage(); err != nil || string(msg) != "after-resume" {
		t.Fatalf("relay not working after resume: %v %q", err, msg)
	}
}

func enrollDevice(t *testing.T, base, name string) struct {
	DeviceID    string `json:"device_id"`
	DeviceToken string `json:"device_token"`
} {
	t.Helper()
	_, body := adminJSON(t, http.MethodPost, base+"/v1/enroll-tokens", `{}`)
	var tok struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(body, &tok)
	_, body = adminJSON(t, http.MethodPost, base+"/v1/enroll", `{"token":"`+tok.Token+`","name":"`+name+`"}`)
	var dev struct {
		DeviceID    string `json:"device_id"`
		DeviceToken string `json:"device_token"`
	}
	_ = json.Unmarshal(body, &dev)
	if dev.DeviceToken == "" {
		t.Fatalf("enroll failed: %s", body)
	}
	return dev
}

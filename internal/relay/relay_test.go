package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsPairs returns a server-side connection pair plus the two client peers.
// relay.Pipe(a, b) forwards what peerA writes to peerB and vice versa.
func wsPairs(t *testing.T) (a, b, peerA, peerB *websocket.Conn) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	aCh := make(chan *websocket.Conn, 1)
	bCh := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		switch r.URL.Path {
		case "/a":
			aCh <- conn
		case "/b":
			bCh <- conn
		default:
			conn.Close()
		}
	}))
	t.Cleanup(srv.Close)

	wsBase := "ws" + strings.TrimPrefix(srv.URL, "http")
	peerA, _, errA := websocket.DefaultDialer.Dial(wsBase+"/a", nil)
	if errA != nil {
		t.Fatalf("dial a: %v", errA)
	}
	peerB, _, errB := websocket.DefaultDialer.Dial(wsBase+"/b", nil)
	if errB != nil {
		t.Fatalf("dial b: %v", errB)
	}
	t.Cleanup(func() { peerA.Close(); peerB.Close() })
	return <-aCh, <-bCh, peerA, peerB
}

func TestPipeForwardsMessages(t *testing.T) {
	a, b, peerA, peerB := wsPairs(t)
	go Pipe(a, b)

	if err := peerA.WriteMessage(websocket.TextMessage, []byte("hello-cdp")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = peerB.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, msg, err := peerB.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if mt != websocket.TextMessage || string(msg) != "hello-cdp" {
		t.Fatalf("got type %d payload %q", mt, msg)
	}

	// Binary frames keep their type.
	if err := peerB.WriteMessage(websocket.BinaryMessage, []byte{0x01, 0x02}); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	_ = peerA.SetReadDeadline(time.Now().Add(5 * time.Second))
	mt, msg, err = peerA.ReadMessage()
	if err != nil || mt != websocket.BinaryMessage || len(msg) != 2 {
		t.Fatalf("binary passthrough failed: type=%d len=%d err=%v", mt, len(msg), err)
	}
}

// A frame larger than the limit must close the relay rather than be buffered.
func TestPipeClosesOnOversizedMessage(t *testing.T) {
	old := MaxMessageBytes
	MaxMessageBytes = 1024
	defer func() { MaxMessageBytes = old }()

	a, b, peerA, _ := wsPairs(t)
	done := make(chan error, 1)
	go func() { done <- Pipe(a, b) }()

	if err := peerA.WriteMessage(websocket.TextMessage, make([]byte, 4096)); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not close after an oversized frame")
	}
}

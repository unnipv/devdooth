package worker

import (
	"bufio"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/unnipv/devdooth/internal/protocol"
)

// newTestWorker builds a worker without probing the host for browsers, so the
// tests are fast and hermetic. No lease is created, so no browser is launched.
func newTestWorker(t *testing.T, coordinatorURL string) *Worker {
	t.Helper()
	return &Worker{
		opts: Options{
			CoordinatorURL: coordinatorURL,
			Token:          "test",
			Name:           "n",
			DataDir:        t.TempDir(),
		},
		log:          log.New(io.Discard, "", 0),
		generation:   "test",
		browsers:     []protocol.Browser{{Name: "chrome", Path: "/nonexistent"}},
		leases:       map[string]*running{},
		profileLocks: map[string]string{},
	}
}

// A cancelled context (SIGTERM in the CLI) must stop a connected worker
// promptly. Regression test: a blocked control-channel read used to ignore
// cancellation until the 90s read deadline, leaving the node registered.
func TestRunStopsOnContextCancel(t *testing.T) {
	connected := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil { // hello
			return
		}
		connected <- struct{}{}
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	w := newTestWorker(t, srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never connected")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop within 3s of context cancellation")
	}
}

// A coordinator that accepts TCP but never answers the WebSocket upgrade must
// not delay shutdown. Gorilla reads the upgrade response from the raw socket,
// which a cancelled DialContext alone does not interrupt.
func TestRunStopsWhenHandshakeStalls(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan struct{}, 1)
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			// Read the upgrade request so the client is provably blocked waiting
			// for a response before we allow cancellation.
			if _, err := http.ReadRequest(bufio.NewReader(conn)); err != nil {
				continue
			}
			select {
			case accepted <- struct{}{}:
			default:
			}
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})

	w := newTestWorker(t, "http://"+ln.Addr().String())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never started dialing")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not stop within 3s while the handshake was stalled")
	}
}

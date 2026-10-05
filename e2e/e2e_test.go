// Package e2e contains end-to-end tests that exercise a real Chromium through
// the full Devdooth relay path: raw CDP client -> coordinator -> worker
// tunnel -> browser.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/unnipv/devdooth/internal/coordinator"
	"github.com/unnipv/devdooth/internal/worker"
)

const (
	adminToken  = "e2e-admin"
	workerToken = "e2e-worker"
)

func TestRealChromiumOverRelay(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end browser test in short mode")
	}
	base, node := startStack(t, 1)
	lease := acquire(t, base, fmt.Sprintf(`{"node":%q,"ttl_seconds":120}`, node))
	defer release(t, base, lease.LeaseID)

	calls := dialCDP(t, lease.Endpoint)
	defer calls.conn.Close()

	version := calls.call("Browser.getVersion", nil, "")
	product, _ := version["product"].(string)
	if product == "" {
		t.Fatalf("Browser.getVersion returned no product: %v", version)
	}
	t.Logf("connected to %s", product)

	targetID := calls.newTarget("about:blank")
	sessionID := calls.attach(targetID)
	calls.call("Page.enable", nil, sessionID)
	calls.call("Page.navigate", map[string]any{"url": "data:text/html,<title>devdooth-e2e</title><h1>hi</h1>"}, sessionID)

	title := waitFor(t, 10*time.Second, func() string {
		return calls.eval(sessionID, "document.title")
	})
	if title != "devdooth-e2e" {
		t.Fatalf("expected page title devdooth-e2e, got %q", title)
	}
}

// TestPersistentProfileSurvivesLeases proves that browser state is stored on
// the worker and reused across separate leases.
func TestPersistentProfileSurvivesLeases(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end browser test in short mode")
	}
	base, node := startStack(t, 1, "shopping")

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<title>origin</title>ok")
	}))
	defer origin.Close()

	// First lease: write localStorage.
	l1 := acquire(t, base, fmt.Sprintf(`{"node":%q,"profile":"shopping","ttl_seconds":120}`, node))
	c1 := dialCDP(t, l1.Endpoint)
	sid1 := c1.openPage(origin.URL)
	if got := c1.eval(sid1, `localStorage.setItem('devdooth','abc-123'); localStorage.getItem('devdooth')`); got != "abc-123" {
		t.Fatalf("failed to write localStorage, got %q", got)
	}
	c1.conn.Close()
	release(t, base, l1.LeaseID)

	// Second lease on the same profile: the value must still be there.
	l2 := acquire(t, base, fmt.Sprintf(`{"node":%q,"profile":"shopping","ttl_seconds":120}`, node))
	c2 := dialCDP(t, l2.Endpoint)
	sid2 := c2.openPage(origin.URL)
	if got := c2.eval(sid2, `localStorage.getItem('devdooth')`); got != "abc-123" {
		t.Fatalf("profile state did not persist across leases, got %q", got)
	}
	c2.conn.Close()
	release(t, base, l2.LeaseID)
}

// TestProfileIsExclusivelyLocked proves one profile is not opened by two
// browsers at once, even when the node has spare capacity.
func TestProfileIsExclusivelyLocked(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping end-to-end browser test in short mode")
	}
	base, node := startStack(t, 2, "shopping")

	l1 := acquire(t, base, fmt.Sprintf(`{"node":%q,"profile":"shopping","ttl_seconds":120}`, node))
	defer release(t, base, l1.LeaseID)

	_, code, body := acquireRaw(t, base, fmt.Sprintf(`{"node":%q,"profile":"shopping","ttl_seconds":120}`, node))
	if code == http.StatusOK {
		t.Fatalf("expected the second lease on the same profile to be rejected, got %d %s", code, body)
	}
}

// ---------------------------------------------------------------- stack

func startStack(t *testing.T, slots int, profiles ...string) (base, node string) {
	t.Helper()
	probe := worker.New(worker.Options{CoordinatorURL: "http://localhost", Token: "x", DataDir: t.TempDir()})
	if len(probe.Browsers()) == 0 {
		t.Skip("no Chromium-family browser installed")
	}

	c := coordinator.New(coordinator.Options{AdminToken: adminToken, WorkerToken: workerToken})
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	node = "e2e-node"
	w := worker.New(worker.Options{
		CoordinatorURL: srv.URL,
		Token:          workerToken,
		Name:           node,
		DataDir:        t.TempDir(),
		MaxSlots:       slots,
		Profiles:       profiles,
		// Containers and locked-down CI runners restrict unprivileged user
		// namespaces, which makes Chrome's sandbox exit immediately.
		NoSandbox: os.Getenv("DEVDOOTH_E2E_NO_SANDBOX") != "",
	})
	go func() { _ = w.Run(ctx) }()
	waitForNode(t, srv.URL, node, 30*time.Second)
	return srv.URL, node
}

// ---------------------------------------------------------------- CDP client

type client struct {
	t    *testing.T
	conn *websocket.Conn
	mu   sync.Mutex
	next int
}

func dialCDP(t *testing.T, endpoint string) *client {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(endpoint, nil)
	if err != nil {
		t.Fatalf("dial CDP endpoint: %v", err)
	}
	return &client{t: t, conn: conn}
}

func (c *client) call(method string, params map[string]any, sessionID string) map[string]any {
	c.t.Helper()
	c.mu.Lock()
	c.next++
	id := c.next
	c.mu.Unlock()

	msg := map[string]any{"id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if sessionID != "" {
		msg["sessionId"] = sessionID
	}
	if err := c.conn.WriteJSON(msg); err != nil {
		c.t.Fatalf("write %s: %v", method, err)
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			c.t.Fatalf("read response to %s: %v", method, err)
		}
		var reply map[string]any
		if err := json.Unmarshal(data, &reply); err != nil {
			continue
		}
		gotID, ok := reply["id"].(float64)
		if !ok || int(gotID) != id {
			continue // CDP event
		}
		if errObj, ok := reply["error"]; ok {
			c.t.Fatalf("%s returned error: %v", method, errObj)
		}
		result, _ := reply["result"].(map[string]any)
		if result == nil {
			result = map[string]any{}
		}
		return result
	}
}

func (c *client) newTarget(url string) string {
	created := c.call("Target.createTarget", map[string]any{"url": url}, "")
	targetID := digString(created, "targetId")
	if targetID == "" {
		c.t.Fatalf("Target.createTarget returned no targetId: %v", created)
	}
	return targetID
}

func (c *client) attach(targetID string) string {
	attached := c.call("Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, "")
	sessionID := digString(attached, "sessionId")
	if sessionID == "" {
		c.t.Fatalf("Target.attachToTarget returned no sessionId: %v", attached)
	}
	return sessionID
}

func (c *client) openPage(url string) string {
	c.t.Helper()
	sessionID := c.attach(c.newTarget("about:blank"))
	c.call("Page.enable", nil, sessionID)
	c.call("Page.navigate", map[string]any{"url": url}, sessionID)
	waitFor(c.t, 10*time.Second, func() string { return c.eval(sessionID, "document.readyState") })
	return sessionID
}

func (c *client) eval(sessionID, expr string) string {
	r := c.call("Runtime.evaluate", map[string]any{"expression": expr, "returnByValue": true}, sessionID)
	return digString(r, "result", "value")
}

func waitFor(t *testing.T, timeout time.Duration, probe func() string) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		last = probe()
		if last != "" {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	return last
}

func digString(m map[string]any, path ...string) string {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[p]
	}
	s, _ := cur.(string)
	return s
}

// ---------------------------------------------------------------- coordinator API

type leaseInfo struct {
	LeaseID  string `json:"lease_id"`
	State    string `json:"state"`
	Endpoint string `json:"endpoint"`
}

func acquire(t *testing.T, base, body string) leaseInfo {
	t.Helper()
	out, code, raw := acquireRaw(t, base, body)
	if code != http.StatusOK {
		t.Fatalf("acquire status %d: %s", code, raw)
	}
	return out
}

func acquireRaw(t *testing.T, base, body string) (leaseInfo, int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/leases", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out leaseInfo
	_ = json.Unmarshal(b, &out)
	return out, resp.StatusCode, string(b)
}

func release(t *testing.T, base, id string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, base+"/v1/leases/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release status %d", resp.StatusCode)
	}
}

func waitForNode(t *testing.T, base, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, base+"/v1/nodes", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			var out struct {
				Nodes []struct {
					Name   string `json:"name"`
					Online bool   `json:"online"`
				} `json:"nodes"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&out)
			resp.Body.Close()
			for _, n := range out.Nodes {
				if n.Name == name && n.Online {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("node %q did not come online", name)
}

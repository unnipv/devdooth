package coordinator_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/unnipv/devdooth/internal/coordinator"
	"github.com/unnipv/devdooth/internal/protocol"
)

func newBareServer(t *testing.T) *httptest.Server {
	t.Helper()
	c := coordinator.New(coordinator.Options{AdminToken: "admin-token", WorkerToken: "worker-token"})
	srv := httptest.NewServer(c.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func waitForNodes(t *testing.T, base string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if nodeCount(t, base) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected at least %d nodes, got %d", want, nodeCount(t, base))
}

func releaseLease(t *testing.T, base, id string) {
	t.Helper()
	if code, _ := adminJSON(t, http.MethodDelete, base+"/v1/leases/"+id, ""); code != http.StatusOK {
		t.Fatalf("release %s: %d", id, code)
	}
}

// A request is routed to the node that matches its constraints.
func TestSchedulingRoutesToMatchingNode(t *testing.T) {
	srv := newBareServer(t)
	mac := newFakeWorker(t, srv.URL, func(h *protocol.Hello) {
		h.Name = "macbook"
		h.Headful = true
		h.MaxSlots = 1
		h.Browsers = []protocol.Browser{{Name: "chrome", Path: "/fake/chrome"}}
		h.Profiles = []string{"shopping"}
	})
	pi := newFakeWorker(t, srv.URL, func(h *protocol.Hello) {
		h.Name = "bedroom-pi"
		h.Headful = false
		h.MaxSlots = 1
		h.Browsers = []protocol.Browser{{Name: "chromium", Path: "/fake/chromium"}}
		h.Profiles = []string{"pi-shopping"}
	})
	defer mac.conn.Close()
	defer pi.conn.Close()
	waitForNodes(t, srv.URL, 2)

	cases := []struct {
		name string
		body string
		want string
	}{
		{"headful picks the capable node", `{"headful":true}`, "macbook"},
		{"browser picks the matching node", `{"browser":"chromium"}`, "bedroom-pi"},
		{"profile affinity", `{"profile":"pi-shopping"}`, "bedroom-pi"},
		{"explicit node", `{"node":"macbook"}`, "macbook"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lease := acquireLease(t, srv.URL, tc.body)
			if lease.Node != tc.want {
				t.Fatalf("routed to %q, want %q", lease.Node, tc.want)
			}
			releaseLease(t, srv.URL, lease.LeaseID)
		})
	}

	// An impossible request is refused with a clear error.
	if _, code, body := acquireLeaseRaw(t, srv.URL, `{"browser":"firefox"}`); code == http.StatusOK {
		t.Fatalf("expected failure for an unavailable browser, got %s", body)
	} else if !strings.Contains(body, "firefox") {
		t.Fatalf("error should name the missing browser: %s", body)
	}
}

// A node whose control connection goes away is no longer scheduled, and the
// refusal is a scheduling conflict rather than a launch failure.
func TestOfflineNodeIsNotScheduled(t *testing.T) {
	srv := newBareServer(t)
	fw := newFakeWorker(t, srv.URL, func(h *protocol.Hello) {
		h.Name = "only-node"
		h.MaxSlots = 1
	})
	waitForNodes(t, srv.URL, 1)

	// A healthy lease first, then release it so the slot is free again.
	first := acquireLease(t, srv.URL, `{}`)
	releaseLease(t, srv.URL, first.LeaseID)

	fw.conn.Close()

	// Wait until the coordinator knows the node is offline.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && nodeOnline(t, srv.URL, "only-node") {
		time.Sleep(20 * time.Millisecond)
	}
	if nodeOnline(t, srv.URL, "only-node") {
		t.Fatal("node was still reported online after its connection closed")
	}

	_, code, body := acquireLeaseRaw(t, srv.URL, `{}`)
	if code != http.StatusConflict {
		t.Fatalf("expected a 409 scheduling refusal, got %d: %s", code, body)
	}
}

func nodeOnline(t *testing.T, base, name string) bool {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var out struct {
		Nodes []struct {
			Name   string `json:"name"`
			Online bool   `json:"online"`
		} `json:"nodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false
	}
	for _, n := range out.Nodes {
		if n.Name == name {
			return n.Online
		}
	}
	return false
}

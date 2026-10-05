package coordinator

import (
	"testing"

	"github.com/unnipv/devdooth/internal/protocol"
)

func testCoordinator() *Coordinator {
	return New(Options{AdminToken: "admin", WorkerToken: "worker"})
}

func addNode(c *Coordinator, n *Node) {
	if n.MaxSlots == 0 {
		n.MaxSlots = 1
	}
	n.online = true
	n.send = make(chan []byte, 8)
	n.closed = make(chan struct{})
	c.nodes[n.ID] = n
}

func TestSelectNodeFilters(t *testing.T) {
	mac := &Node{ID: "macbook", Name: "macbook", Headful: true,
		Browsers: []protocol.Browser{{Name: "chrome"}}, Profiles: []string{"shopping"}}
	pi := &Node{ID: "bedroom-pi", Name: "bedroom-pi", Headful: false,
		Browsers: []protocol.Browser{{Name: "chromium"}}, Profiles: []string{"clean"}}

	c := testCoordinator()
	addNode(c, mac)
	addNode(c, pi)

	tests := []struct {
		name    string
		req     leaseRequest
		want    string
		wantErr bool
	}{
		{"profile routes to owner node", leaseRequest{Profile: "shopping"}, "macbook", false},
		{"unknown profile rejected", leaseRequest{Profile: "nope"}, "", true},
		{"headful requires capable node", leaseRequest{Headful: true}, "macbook", false},
		{"browser match", leaseRequest{Browser: "chromium"}, "bedroom-pi", false},
		{"unknown browser rejected", leaseRequest{Browser: "firefox"}, "", true},
		{"explicit node", leaseRequest{Node: "bedroom-pi"}, "bedroom-pi", false},
		{"unknown node rejected", leaseRequest{Node: "ghost"}, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.selectNode(tc.req)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got node %q", got.Name)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Name != tc.want {
				t.Fatalf("got %q, want %q", got.Name, tc.want)
			}
		})
	}
}

func TestSelectNodeSkipsFullAndOffline(t *testing.T) {
	a := &Node{ID: "a", Name: "a", Browsers: []protocol.Browser{{Name: "chrome"}}, MaxSlots: 1, active: 1}
	b := &Node{ID: "b", Name: "b", Browsers: []protocol.Browser{{Name: "chrome"}}, MaxSlots: 2}
	cnode := &Node{ID: "c", Name: "c", Browsers: []protocol.Browser{{Name: "chrome"}}, MaxSlots: 1}
	c := testCoordinator()
	addNode(c, a)
	addNode(c, b)
	addNode(c, cnode)
	cnode.online = false

	got, err := c.selectNode(leaseRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "b" {
		t.Fatalf("expected least-loaded eligible node b, got %q", got.Name)
	}

	// Fill b as well; only the full/offline nodes remain.
	b.active = 2
	if _, err := c.selectNode(leaseRequest{}); err == nil {
		t.Fatal("expected no eligible node")
	}
}

func TestSubtleEqual(t *testing.T) {
	if !subtleEqual("abc", "abc") {
		t.Fatal("equal strings should match")
	}
	for _, tc := range [][2]string{{"abc", "abd"}, {"abc", "ab"}, {"", ""}, {"abc", ""}} {
		if subtleEqual(tc[0], tc[1]) {
			t.Fatalf("expected mismatch for %q vs %q", tc[0], tc[1])
		}
	}
}

func TestTokenEntropyUniqueness(t *testing.T) {
	c := testCoordinator()
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		tok := c.newLeaseToken()
		if seen[tok] {
			t.Fatal("duplicate lease token generated")
		}
		seen[tok] = true
	}
}

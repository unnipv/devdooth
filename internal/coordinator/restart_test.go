package coordinator_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/unnipv/devdooth/internal/coordinator"
)

// Restarting the coordinator loses in-memory leases (they are invalidated), but
// an enrolled device's identity is durable and it can reconnect with the same
// token.
func TestCoordinatorRestartInvalidatesLeases(t *testing.T) {
	dbPath := t.TempDir() + "/devdooth.db"

	store1, err := coordinator.OpenStore(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	c1 := coordinator.New(coordinator.Options{AdminToken: "admin-token", Store: store1})
	srv1 := httptest.NewServer(c1.Handler())

	device := enrollDevice(t, srv1.URL, "restart-node")

	worker1 := &relayWorker{t: t, base: srv1.URL, token: device.DeviceToken, name: "restart-node"}
	worker1.connect()
	waitForNodes(t, srv1.URL, 1)

	lease := acquireLease(t, srv1.URL, `{"node":"restart-node","ttl_seconds":120}`)
	if lease.State != "ready" {
		t.Fatalf("expected a ready lease, got %q", lease.State)
	}

	// The coordinator and its worker connection both go away.
	worker1.close()
	srv1.Close()
	if err := store1.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// It restarts against the same store. In-memory leases are gone.
	store2, err := coordinator.OpenStore(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { store2.Close() })
	c2 := coordinator.New(coordinator.Options{AdminToken: "admin-token", Store: store2})
	srv2 := httptest.NewServer(c2.Handler())
	t.Cleanup(srv2.Close)

	if code, _ := adminJSON(t, http.MethodGet, srv2.URL+"/v1/leases/"+lease.LeaseID, ""); code != http.StatusNotFound {
		t.Fatalf("lease from before the restart should be unknown, got %d", code)
	}

	// The enrolled device reconnects with its durable token and can be leased.
	worker2 := &relayWorker{t: t, base: srv2.URL, token: device.DeviceToken, name: "restart-node"}
	worker2.connect()
	defer worker2.close()
	waitForNodes(t, srv2.URL, 1)

	fresh := acquireLease(t, srv2.URL, `{"node":"restart-node","ttl_seconds":60}`)
	if fresh.State != "ready" {
		t.Fatalf("expected a ready lease after reconnect, got %q", fresh.State)
	}
	releaseLease(t, srv2.URL, fresh.LeaseID)
}

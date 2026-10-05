package coordinator_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/unnipv/devdooth/internal/coordinator"
)

// Restarting the coordinator loses in-memory leases (they are invalidated), but
// enrolled devices are durable and can reconnect with the same identity.
func TestCoordinatorRestartInvalidatesLeases(t *testing.T) {
	store, err := coordinator.OpenStore("")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	newCoordinator := func() *httptest.Server {
		c := coordinator.New(coordinator.Options{
			AdminToken: "admin-token", WorkerToken: "worker-token", Store: store,
		})
		srv := httptest.NewServer(c.Handler())
		t.Cleanup(srv.Close)
		return srv
	}

	before := newCoordinator()
	fw := newFakeWorker(t, before.URL)
	defer fw.conn.Close()
	waitForNodes(t, before.URL, 1)
	lease := acquireLease(t, before.URL, `{"node":"fake","ttl_seconds":120}`)

	// The coordinator restarts: a fresh instance with the same store.
	after := newCoordinator()

	if code, _ := adminJSON(t, http.MethodGet, after.URL+"/v1/leases/"+lease.LeaseID, ""); code != http.StatusNotFound {
		t.Fatalf("lease from before the restart should be unknown, got %d", code)
	}

	// A worker reconnects using its durable identity.
	fw2 := newFakeWorker(t, after.URL)
	defer fw2.conn.Close()
	waitForNodes(t, after.URL, 1)

	// And can still be leased.
	fresh := acquireLease(t, after.URL, `{"node":"fake","ttl_seconds":60}`)
	if fresh.State != "ready" {
		t.Fatalf("expected a ready lease after reconnect, got %q", fresh.State)
	}
	releaseLease(t, after.URL, fresh.LeaseID)
}

package coordinator

import (
	"sync"
	"testing"
	"time"
)

func TestFailNodeLeasesOnlyAffectsItsOwnSession(t *testing.T) {
	old := &Node{ID: "macbook", Name: "macbook"}
	newer := &Node{ID: "macbook", Name: "macbook"}

	c := testCoordinator()
	for _, n := range []*Node{old, newer} {
		n.online = true
		n.send = make(chan []byte, 8)
		n.closed = make(chan struct{})
		n.MaxSlots = 2
	}
	c.nodes[old.ID] = newer // the registered session is the replacement

	oldLease := &Lease{ID: "lse_old", NodeID: old.ID, node: old, State: StateReady, reserved: true, done: make(chan struct{}), stopped: make(chan struct{})}
	newLease := &Lease{ID: "lse_new", NodeID: newer.ID, node: newer, State: StateReady, reserved: true, done: make(chan struct{}), stopped: make(chan struct{})}
	old.active = 1
	newer.active = 1
	c.leases[oldLease.ID] = oldLease
	c.leases[newLease.ID] = newLease

	// The old session disconnects and its leases are failed.
	old.online = false
	c.failNodeLeases(old, "worker reconnected")

	if oldLease.State != StateFailed {
		t.Fatalf("old lease should fail, got %s", oldLease.State)
	}
	if newLease.State != StateReady {
		t.Fatalf("replacement lease must be untouched, got %s", newLease.State)
	}
	if newer.active != 1 {
		t.Fatalf("replacement capacity must be untouched, got %d", newer.active)
	}
}

func TestRelayAdmissionIsRefusedForDeadSession(t *testing.T) {
	c := testCoordinator()
	n := &Node{ID: "macbook", Name: "macbook", online: true}

	if _, ok := c.addRelay(n, func() {}); !ok {
		t.Fatal("a live session should accept a relay")
	}

	// Marking the session dead must refuse later admissions.
	c.mu.Lock()
	relays := c.takeRelaysLocked(n)
	c.mu.Unlock()
	closeAll(relays)

	if _, ok := c.addRelay(n, func() {}); ok {
		t.Fatal("a dead session must not admit new relays")
	}
}

func TestRelaysAreSessionScoped(t *testing.T) {
	c := testCoordinator()
	first := &Node{ID: "macbook", Name: "macbook", online: true}
	second := &Node{ID: "macbook", Name: "macbook", online: true}

	closed := map[string]bool{}
	if _, ok := c.addRelay(first, func() { closed["first"] = true }); !ok {
		t.Fatal("first relay rejected")
	}
	if _, ok := c.addRelay(second, func() { closed["second"] = true }); !ok {
		t.Fatal("second relay rejected")
	}

	// Only the second session ends.
	c.mu.Lock()
	relays := c.takeRelaysLocked(second)
	c.mu.Unlock()
	closeAll(relays)

	if closed["first"] {
		t.Fatal("ending one session must not close another session's relays")
	}
	if !closed["second"] {
		t.Fatal("the ending session's relay should be closed")
	}
}

func TestAttachmentAcceptsOneTunnel(t *testing.T) {
	a := &attachment{ready: make(chan struct{})}

	if !a.deliver(nil) {
		t.Fatal("first delivery should be accepted")
	}
	if a.deliver(nil) {
		t.Fatal("a duplicate tunnel must be rejected so it can be closed")
	}

	a.cancel()
	if !a.closed {
		t.Fatal("cancel should mark the attachment closed")
	}
	if a.deliver(nil) {
		t.Fatal("delivery after cancellation must be rejected")
	}
}

func TestWSScheme(t *testing.T) {
	cases := map[string]string{
		"https://coord.example":  "wss://coord.example",
		"https://coord.example/": "wss://coord.example",
		"http://localhost:8080":  "ws://localhost:8080",
		"ws://coord.example":     "ws://coord.example",
		"wss://coord.example":    "wss://coord.example",
	}
	for in, want := range cases {
		if got := wsScheme(in); got != want {
			t.Fatalf("wsScheme(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPublishAttachmentRefusedWhilePaused(t *testing.T) {
	lease := &Lease{ID: "lse_1", done: make(chan struct{}), stopped: make(chan struct{})}
	a := &attachment{id: "att_1", ready: make(chan struct{})}

	if !lease.publishAttachment(a) {
		t.Fatal("a running lease should publish an attachment")
	}
	if lease.attachment != a {
		t.Fatal("attachment was not registered")
	}

	// Pausing clears the way for a new controller, then a racing setup must be
	// refused rather than silently published.
	lease.attachMu.Lock()
	lease.paused = true
	lease.attachment = nil
	lease.attachMu.Unlock()

	if lease.publishAttachment(&attachment{id: "att_2", ready: make(chan struct{})}) {
		t.Fatal("publish must be refused while paused")
	}
	if lease.attachment != nil {
		t.Fatal("a refused publish must not register an attachment")
	}
}

func TestOwnedLeaseRejectsOtherSessions(t *testing.T) {
	c := testCoordinator()
	n := &Node{ID: "macbook", Name: "macbook", online: true, send: make(chan []byte, 4), closed: make(chan struct{})}
	other := &Node{ID: "other", Name: "other", online: true, send: make(chan []byte, 4), closed: make(chan struct{})}
	c.leases["lse_1"] = &Lease{ID: "lse_1", NodeID: n.ID, node: n, done: make(chan struct{}), stopped: make(chan struct{})}

	if c.ownedLease(n, "lse_1") == nil {
		t.Fatal("owner should be able to resolve its lease")
	}
	if c.ownedLease(other, "lse_1") != nil {
		t.Fatal("a different session must not resolve someone else's lease")
	}
	if c.ownedLease(n, "lse_missing") != nil {
		t.Fatal("unknown lease must not resolve")
	}
}

func TestAdminTokenIsNotAWorkerTokenWithStore(t *testing.T) {
	store, err := OpenStore("")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	c := New(Options{AdminToken: "admin-secret", Store: store})
	if c.opts.WorkerToken != "" {
		t.Fatalf("worker token must stay empty in durable mode, got %q", c.opts.WorkerToken)
	}
}

func TestConcurrentEnrollmentRedemptionIsSingleUse(t *testing.T) {
	s := newTestStore(t)
	_, token, _, err := s.CreateEnrollToken("", time.Hour)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	const attempts = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		success int
		failure int
	)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := s.RedeemEnrollToken(token, "node-"+string(rune('a'+i)))
			mu.Lock()
			if err == nil {
				success++
			} else {
				failure++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if success != 1 {
		t.Fatalf("exactly one redemption should succeed, got %d (failures %d)", success, failure)
	}
	if failure != attempts-1 {
		t.Fatalf("expected %d failures, got %d", attempts-1, failure)
	}
}

func TestStoreDurabilityAcrossReopen(t *testing.T) {
	path := t.TempDir() + "/devdooth.db"
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, token, _, _ := s.CreateEnrollToken("", time.Hour)
	device, deviceToken, err := s.RedeemEnrollToken(token, "macbook")
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if err := s.RevokeDevice(device.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	// Revocation and the consumed token survived the restart.
	if _, err := reopened.AuthenticateDevice(deviceToken); err == nil {
		t.Fatal("revoked device must stay revoked after reopen")
	}
	if _, _, err := reopened.RedeemEnrollToken(token, "again"); err == nil {
		t.Fatal("consumed enrollment token must stay consumed after reopen")
	}
	devices, err := reopened.ListDevices()
	if err != nil || len(devices) != 1 || devices[0].Name != "macbook" {
		t.Fatalf("device row did not persist: %+v (%v)", devices, err)
	}
}

package coordinator

import (
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore("")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestEnrollTokenIsSingleUse(t *testing.T) {
	s := newTestStore(t)
	_, token, _, err := s.CreateEnrollToken("laptop", time.Hour)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	device, deviceToken, err := s.RedeemEnrollToken(token, "macbook")
	if err != nil {
		t.Fatalf("first redeem: %v", err)
	}
	if device.Name != "macbook" || deviceToken == "" {
		t.Fatalf("unexpected device: %+v token=%q", device, deviceToken)
	}

	if _, _, err := s.RedeemEnrollToken(token, "other"); err == nil {
		t.Fatal("expected replay of the enrollment token to fail")
	}

	got, err := s.AuthenticateDevice(deviceToken)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if got.ID != device.ID || got.Name != "macbook" {
		t.Fatalf("authenticated to wrong device: %+v", got)
	}
}

func TestEnrollTokenExpiry(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	s.now = func() time.Time { return now }

	_, token, _, err := s.CreateEnrollToken("", time.Minute)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	now = now.Add(2 * time.Minute)
	if _, _, err := s.RedeemEnrollToken(token, "late"); err == nil {
		t.Fatal("expected expired token to be rejected")
	}
}

func TestDeviceRevocation(t *testing.T) {
	s := newTestStore(t)
	_, token, _, _ := s.CreateEnrollToken("", time.Hour)
	device, deviceToken, _ := s.RedeemEnrollToken(token, "pi")

	if _, err := s.AuthenticateDevice(deviceToken); err != nil {
		t.Fatalf("device should authenticate: %v", err)
	}
	if err := s.RevokeDevice(device.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.AuthenticateDevice(deviceToken); err == nil {
		t.Fatal("revoked device must not authenticate")
	}
	if err := s.RevokeDevice("dev_missing"); err == nil {
		t.Fatal("revoking an unknown device should fail")
	}
}

func TestDeviceNameIsUnique(t *testing.T) {
	s := newTestStore(t)
	_, t1, _, _ := s.CreateEnrollToken("", time.Hour)
	_, t2, _, _ := s.CreateEnrollToken("", time.Hour)
	if _, _, err := s.RedeemEnrollToken(t1, "macbook"); err != nil {
		t.Fatalf("first enroll: %v", err)
	}
	if _, _, err := s.RedeemEnrollToken(t2, "macbook"); err == nil {
		t.Fatal("expected duplicate device name to be rejected")
	}
}

func TestUnknownDeviceTokenRejected(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.AuthenticateDevice("not-a-real-token"); err == nil {
		t.Fatal("expected unknown token to be rejected")
	}
	if _, err := s.AuthenticateDevice(""); err == nil {
		t.Fatal("expected empty token to be rejected")
	}
}

func TestListDevices(t *testing.T) {
	s := newTestStore(t)
	for _, name := range []string{"macbook", "pi"} {
		_, tok, _, _ := s.CreateEnrollToken("", time.Hour)
		if _, _, err := s.RedeemEnrollToken(tok, name); err != nil {
			t.Fatalf("enroll %s: %v", name, err)
		}
	}
	devices, err := s.ListDevices()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(devices) != 2 || devices[0].Name != "macbook" || devices[1].Name != "pi" {
		t.Fatalf("unexpected devices: %+v", devices)
	}
}

func TestValidateDeviceName(t *testing.T) {
	valid := []string{"macbook", "bedroom-pi", "node_1", "a.b"}
	for _, n := range valid {
		if err := validateDeviceName(n); err != nil {
			t.Fatalf("%q should be valid: %v", n, err)
		}
	}
	invalid := []string{"", "has space", "emoji😀", "slash/name", string(make([]byte, 65))}
	for _, n := range invalid {
		if err := validateDeviceName(n); err == nil {
			t.Fatalf("%q should be invalid", n)
		}
	}
}

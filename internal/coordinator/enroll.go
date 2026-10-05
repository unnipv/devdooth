package coordinator

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// maxEnrollTTLSeconds bounds an enrollment token's lifetime.
const maxEnrollTTLSeconds = 7 * 24 * 60 * 60

// Enrollment turns a short-lived, single-use token into a durable device
// identity. The plaintext device token is returned exactly once and stored by
// the worker; the coordinator keeps only its hash.

type enrollTokenRequest struct {
	Label      string `json:"label,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

type enrollTokenResponse struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

type enrollRequest struct {
	Token string `json:"token"`
	Name  string `json:"name"`
}

type enrollResponse struct {
	DeviceID    string `json:"device_id"`
	DeviceToken string `json:"device_token"`
	Name        string `json:"name"`
}

// requireStore returns false and writes 503 when durable identity is disabled.
func (c *Coordinator) requireStore(w http.ResponseWriter) bool {
	if c.store == nil {
		httpError(w, http.StatusServiceUnavailable, "this coordinator has no store; durable device identity is disabled")
		return false
	}
	return true
}

func (c *Coordinator) handleCreateEnrollToken(w http.ResponseWriter, r *http.Request) {
	if !c.requireStore(w) {
		return
	}
	var req enrollTokenRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.TTLSeconds < 0 || req.TTLSeconds > maxEnrollTTLSeconds {
		httpError(w, http.StatusBadRequest, fmt.Sprintf("ttl_seconds must be between 0 and %d", maxEnrollTTLSeconds))
		return
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if req.TTLSeconds == 0 {
		ttl = time.Hour
	}
	id, token, expires, err := c.store.CreateEnrollToken(req.Label, ttl)
	if err != nil {
		c.logf("create enrollment token failed: %v", err)
		httpError(w, http.StatusInternalServerError, "could not create enrollment token")
		return
	}
	writeJSON(w, http.StatusCreated, enrollTokenResponse{
		ID: id, Token: token, ExpiresAt: expires.UTC().Format(time.RFC3339),
	})
}

// handleEnroll is unauthenticated by design: possession of a valid enroll token
// is the credential. The token is consumed atomically.
func (c *Coordinator) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if !c.requireStore(w) {
		return
	}
	var req enrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	name := strings.TrimSpace(req.Name)
	if err := validateDeviceName(name); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	device, token, err := c.store.RedeemEnrollToken(req.Token, name)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidEnrollToken),
			errors.Is(err, ErrEnrollTokenUsed),
			errors.Is(err, ErrEnrollTokenExpired):
			// One message for unknown, used, and expired tokens.
			httpError(w, http.StatusUnauthorized, "invalid or expired enrollment token")
		case errors.Is(err, ErrDeviceNameTaken):
			httpError(w, http.StatusConflict, "device name is already enrolled")
		default:
			c.logf("enrollment failed: %v", err)
			httpError(w, http.StatusInternalServerError, "enrollment failed")
		}
		return
	}
	c.logf("device %q enrolled (%s)", device.Name, device.ID)
	writeJSON(w, http.StatusCreated, enrollResponse{
		DeviceID: device.ID, DeviceToken: token, Name: device.Name,
	})
}

type deviceView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	LastSeen  string `json:"last_seen,omitempty"`
	Revoked   bool   `json:"revoked"`
	Online    bool   `json:"online"`
}

func (c *Coordinator) handleListDevices(w http.ResponseWriter, _ *http.Request) {
	if !c.requireStore(w) {
		return
	}
	devices, err := c.store.ListDevices()
	if err != nil {
		httpError(w, http.StatusInternalServerError, "could not list devices")
		return
	}
	c.mu.Lock()
	online := make(map[string]bool)
	for _, n := range c.nodes {
		if n.DeviceID != "" && n.online {
			online[n.DeviceID] = true
		}
	}
	c.mu.Unlock()

	views := make([]deviceView, 0, len(devices))
	for _, d := range devices {
		v := deviceView{
			ID: d.ID, Name: d.Name, Revoked: d.Revoked, Online: online[d.ID],
			CreatedAt: d.CreatedAt.UTC().Format(time.RFC3339),
		}
		if !d.LastSeen.IsZero() {
			v.LastSeen = d.LastSeen.UTC().Format(time.RFC3339)
		}
		views = append(views, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": views})
}

func (c *Coordinator) handleRevokeDevice(w http.ResponseWriter, r *http.Request) {
	if !c.requireStore(w) {
		return
	}
	id := r.PathValue("id")
	if err := c.store.RevokeDevice(id); err != nil {
		if errors.Is(err, ErrDeviceNotFound) {
			httpError(w, http.StatusNotFound, "no such device")
			return
		}
		c.logf("revoke device %s failed: %v", id, err)
		httpError(w, http.StatusInternalServerError, "could not revoke device")
		return
	}
	c.disconnectDevice(id)
	writeJSON(w, http.StatusOK, map[string]string{"device_id": id, "status": "revoked"})
}

// disconnectDevice closes any live control connection for a device and stops
// its in-flight relays.
func (c *Coordinator) disconnectDevice(deviceID string) {
	c.mu.Lock()
	c.revokedDevices[deviceID] = struct{}{}
	var victims []*Node
	var victimsRelays []func()
	for _, n := range c.nodes {
		if n.DeviceID == deviceID && n.online {
			victims = append(victims, n)
			victimsRelays = append(victimsRelays, c.takeRelaysLocked(n)...)
		}
	}
	c.mu.Unlock()
	// Close relays before the control socket so an active controller is dropped
	// even though CDP traffic does not traverse the control channel.
	closeAll(victimsRelays)
	for _, n := range victims {
		c.logf("disconnecting revoked device %q", n.Name)
		n.close()
	}
}

// validateDeviceName keeps node identity predictable and safe to log.
func validateDeviceName(name string) error {
	if name == "" {
		return errNameRequired
	}
	if len(name) > 64 {
		return errNameTooLong
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return errNameChars
		}
	}
	return nil
}

type nameError string

func (e nameError) Error() string { return string(e) }

const (
	errNameRequired nameError = "device name is required"
	errNameTooLong  nameError = "device name must be 64 characters or fewer"
	errNameChars    nameError = "device name may contain only letters, digits, '-', '_' and '.'"
)

// authenticateWorker accepts either the static worker token (development) or an
// enrolled device token. It returns the device when one was used.
func (c *Coordinator) authenticateWorker(r *http.Request) (*Device, bool) {
	tok := bearer(r)
	if c.opts.WorkerToken != "" && subtleEqual(tok, c.opts.WorkerToken) {
		return nil, true
	}
	if c.store != nil && tok != "" {
		if d, err := c.store.AuthenticateDevice(tok); err == nil {
			return d, true
		}
	}
	return nil, false
}

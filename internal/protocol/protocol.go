// Package protocol defines the messages exchanged between the Devdooth
// coordinator and workers over the outbound control WebSocket.
//
// The protocol is intentionally small and browser-agnostic. Devdooth relays
// CDP bytes; it does not understand them.
package protocol

// Control message types.
const (
	// Worker -> coordinator.
	TypeHello      = "hello"
	TypeReady      = "ready"
	TypeLaunchFail = "launch_failed"
	TypeReleased   = "released"
	TypePong       = "pong"

	// Coordinator -> worker.
	TypeLaunch     = "launch"
	TypeRelease    = "release"
	TypeOpenTunnel = "open_tunnel"
	TypePing       = "ping"
)

// Browser describes a browser executable discovered on a worker.
type Browser struct {
	Name    string `json:"name"`    // e.g. "chrome", "chromium"
	Path    string `json:"path"`    // absolute path to the executable
	Version string `json:"version"` // best-effort version string
}

// Hello is the worker's first message after connecting.
type Hello struct {
	Type     string    `json:"type"`
	Name     string    `json:"name"`
	Version  string    `json:"version"`
	OS       string    `json:"os"`
	Arch     string    `json:"arch"`
	Headful  bool      `json:"headful"`
	Browsers []Browser `json:"browsers"`
	Profiles []string  `json:"profiles"`
	MaxSlots int       `json:"max_slots"`
	// Generation is a random value minted at process start. A reconnect with a
	// new generation invalidates any leases the coordinator believed were live.
	Generation string `json:"generation"`
}

// Launch asks a worker to start one browser process for a lease.
type Launch struct {
	Type       string `json:"type"`
	LeaseID    string `json:"lease_id"`
	Browser    string `json:"browser,omitempty"` // requested browser name
	Headful    bool   `json:"headful"`
	Profile    string `json:"profile,omitempty"` // worker-local profile name
	TTLSeconds int    `json:"ttl_seconds"`
	StartURL   string `json:"start_url,omitempty"`
}

// Release asks a worker to stop a browser and free the lease slot.
type Release struct {
	Type    string `json:"type"`
	LeaseID string `json:"lease_id"`
}

// OpenTunnel asks a worker to open one outbound data connection for a client
// attachment. TunnelToken is single-use and bound to AttachmentID.
type OpenTunnel struct {
	Type         string `json:"type"`
	LeaseID      string `json:"lease_id"`
	AttachmentID string `json:"attachment_id"`
	TunnelToken  string `json:"tunnel_token"`
}

// Ready reports that a browser is up and the lease may be attached.
type Ready struct {
	Type    string `json:"type"`
	LeaseID string `json:"lease_id"`
}

// LaunchFail reports that a lease could not be started.
type LaunchFail struct {
	Type    string `json:"type"`
	LeaseID string `json:"lease_id"`
	Error   string `json:"error"`
}

// Released reports that a worker has stopped a browser and freed the slot.
type Released struct {
	Type    string `json:"type"`
	LeaseID string `json:"lease_id"`
}

// Envelope is used by the coordinator to inspect the "type" of any worker
// message before decoding the concrete payload.
type Envelope struct {
	Type string `json:"type"`
}

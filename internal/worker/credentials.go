package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// credentials are the worker's durable device identity, stored on the worker
// machine with 0600 permissions. The coordinator keeps only the token's hash.
type credentials struct {
	DeviceID    string `json:"device_id"`
	DeviceToken string `json:"device_token"`
	Name        string `json:"name"`
	Coordinator string `json:"coordinator,omitempty"`
}

func credentialsPath(dataDir string) string {
	return filepath.Join(dataDir, "device.json")
}

func loadCredentials(dataDir string) (*credentials, error) {
	b, err := os.ReadFile(credentialsPath(dataDir))
	if err != nil {
		return nil, err
	}
	var c credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.DeviceToken == "" || c.Name == "" {
		return nil, fmt.Errorf("incomplete credentials in %s", credentialsPath(dataDir))
	}
	return &c, nil
}

func saveCredentials(dataDir string, c *credentials) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// A uniquely named temp file keeps concurrent enrollments from clobbering
	// each other. CreateTemp creates it 0600; rename is atomic.
	f, err := os.CreateTemp(dataDir, ".device-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once renamed
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, credentialsPath(dataDir))
}

// Enroll consumes the configured enrollment token and persists the resulting
// device identity. It is safe to call once; later runs load the saved identity.
func (w *Worker) Enroll(ctx context.Context) (*credentials, error) {
	if w.opts.EnrollToken == "" {
		return nil, fmt.Errorf("no enrollment token provided")
	}
	name := w.opts.Name
	if name == "" {
		name, _ = os.Hostname()
	}
	if name == "" {
		return nil, fmt.Errorf("a device name is required to enroll")
	}
	// Prepare the destination first: if we cannot store the identity, do not
	// consume the single-use token.
	if err := ensureWritable(w.opts.DataDir); err != nil {
		return nil, fmt.Errorf("data dir %s is not writable: %w", w.opts.DataDir, err)
	}

	base, err := toHTTP(w.opts.CoordinatorURL)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]string{"token": w.opts.EnrollToken, "name": name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/enroll", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("enrollment failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		DeviceID    string `json:"device_id"`
		DeviceToken string `json:"device_token"`
		Name        string `json:"name"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	if out.DeviceToken == "" || out.DeviceID == "" {
		return nil, fmt.Errorf("coordinator returned an incomplete enrollment")
	}
	c := &credentials{
		DeviceID: out.DeviceID, DeviceToken: out.DeviceToken,
		Name: out.Name, Coordinator: w.opts.CoordinatorURL,
	}
	if err := saveCredentials(w.opts.DataDir, c); err != nil {
		// The device exists on the coordinator and the token is consumed. Return
		// the credentials with the error so the caller can persist or print them
		// rather than stranding the identity.
		return c, fmt.Errorf("enrolled as %s but could not save credentials: %w", out.Name, err)
	}
	return c, nil
}

// resolveAuth decides how this worker authenticates: an enrolled device token,
// a fresh enrollment, or a static development token.
func (w *Worker) resolveAuth(ctx context.Context) error {
	if w.opts.DeviceToken != "" {
		w.deviceToken = w.opts.DeviceToken
		return nil
	}
	if c, err := loadCredentials(w.opts.DataDir); err == nil {
		w.deviceToken = c.DeviceToken
		// The enrolled name is authoritative; a stale hostname default must not
		// override the identity the coordinator knows.
		w.opts.Name = c.Name
		if c.Coordinator != "" && normalizeURL(c.Coordinator) != normalizeURL(w.opts.CoordinatorURL) {
			w.logf("warning: stored device identity was enrolled with %s, but this worker points at %s",
				c.Coordinator, w.opts.CoordinatorURL)
		}
		w.logf("using enrolled device identity %q", c.Name)
		return nil
	}
	if w.opts.EnrollToken != "" {
		c, err := w.Enroll(ctx)
		if err != nil {
			if c != nil {
				// Last resort: the device now exists on the coordinator and the
				// enrollment token is spent. Surface the one-time credentials so
				// the identity is recoverable rather than lost.
				w.logf("RECOVERY: enrolled as %q but credentials could not be saved. Store now, shown once: device_id=%s device_token=%s",
					c.Name, c.DeviceID, c.DeviceToken)
			}
			return err
		}
		w.deviceToken = c.DeviceToken
		w.opts.Name = c.Name
		w.logf("enrolled as device %q (%s)", c.Name, c.DeviceID)
		return nil
	}
	if w.opts.Token != "" {
		return nil // static worker token
	}
	return fmt.Errorf("no credentials: pass --enroll-token, an existing device identity in --data-dir, or --token")
}

// authToken is the credential used on the control channel.
func (w *Worker) authToken() string {
	if w.deviceToken != "" {
		return w.deviceToken
	}
	return w.opts.Token
}

// ensureWritable verifies the worker can create files in dir before we spend a
// single-use enrollment token.
func ensureWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

// normalizeURL trims a trailing slash so equivalent URLs compare equal.
func normalizeURL(s string) string {
	return strings.TrimRight(strings.TrimSpace(s), "/")
}

// toHTTP converts a WebSocket coordinator base to its HTTP equivalent.
func toHTTP(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http", "https":
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return "", fmt.Errorf("coordinator URL must be http(s) or ws(s), got %q", base)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

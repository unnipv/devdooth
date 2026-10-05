package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/unnipv/devdooth/internal/worker"
)

// runJoin enrolls this machine and stores a durable device identity. After it
// succeeds, `devdooth worker --coordinator URL` needs no token.
func runJoin(args []string) error {
	fs := newFlagSet("join")
	coord := fs.String("coordinator", "", "coordinator base URL")
	enrollToken := fs.String("enroll-token", env("DEVDOOTH_ENROLL_TOKEN"), "single-use enrollment token")
	name := fs.String("name", "", "device name (defaults to hostname)")
	dataDir := fs.String("data-dir", "", "worker data directory (defaults to ~/.devdooth)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *coord == "" {
		return fmt.Errorf("--coordinator is required")
	}
	if *enrollToken == "" {
		return fmt.Errorf("--enroll-token is required")
	}

	w := worker.New(worker.Options{
		CoordinatorURL: *coord,
		EnrollToken:    *enrollToken,
		Name:           *name,
		DataDir:        *dataDir,
	})
	creds, err := w.Enroll(context.Background())
	if err != nil {
		if creds != nil {
			fmt.Fprintln(os.Stderr, "\nThe device was enrolled but its credentials could not be saved.")
			fmt.Fprintln(os.Stderr, "Store these now; the token is shown only once:")
			fmt.Fprintf(os.Stderr, "  device_id:    %s\n", creds.DeviceID)
			fmt.Fprintf(os.Stderr, "  device_token: %s\n", creds.DeviceToken)
		}
		return err
	}
	fmt.Printf("joined as %s\n", creds.Name)
	fmt.Printf("  device id: %s\n", creds.DeviceID)
	fmt.Printf("  next:      devdooth worker --coordinator %s\n", *coord)
	return nil
}

// runEnrollToken mints a single-use enrollment token.
func runEnrollToken(args []string) error {
	fs := newFlagSet("enroll-token")
	base := fs.String("url", "", "coordinator HTTP base URL")
	coord := fs.String("coordinator", "", "coordinator base URL (http or ws)")
	token := fs.String("token", env("DEVDOOTH_TOKEN"), "admin token")
	label := fs.String("label", "", "human label for the device")
	ttl := fs.Int("ttl", 3600, "token lifetime in seconds")
	if err := fs.Parse(args); err != nil {
		return err
	}
	httpBase, err := resolveBase(*base, *coord)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"label": *label, "ttl_seconds": *ttl})
	body, code, err := apiRequest(http.MethodPost, httpBase+"/v1/enroll-tokens", *token, payload)
	if err != nil {
		return err
	}
	if code != http.StatusCreated {
		return fmt.Errorf("coordinator returned %d: %s", code, strings.TrimSpace(string(body)))
	}
	var out struct {
		ID        string `json:"id"`
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return err
	}
	fmt.Printf("%s\n", out.Token)
	fmt.Fprintf(os.Stderr, "\nEnrollment token (id %s, expires %s). It can be used once:\n", out.ID, out.ExpiresAt)
	fmt.Fprintf(os.Stderr, "  devdooth join --coordinator %s --enroll-token %s --name <device>\n", httpBase, out.Token)
	return nil
}

// runDevices lists or revokes enrolled devices.
func runDevices(args []string) error {
	if len(args) > 0 && args[0] == "revoke" {
		return runDevicesRevoke(args[1:])
	}
	fs := newFlagSet("devices")
	base := fs.String("url", "", "coordinator HTTP base URL")
	coord := fs.String("coordinator", "", "coordinator base URL (http or ws)")
	token := fs.String("token", env("DEVDOOTH_TOKEN"), "admin token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	httpBase, err := resolveBase(*base, *coord)
	if err != nil {
		return err
	}
	body, code, err := apiRequest(http.MethodGet, httpBase+"/v1/devices", *token, nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("coordinator returned %d: %s", code, strings.TrimSpace(string(body)))
	}
	var out struct {
		Devices []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Revoked  bool   `json:"revoked"`
			Online   bool   `json:"online"`
			LastSeen string `json:"last_seen"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return err
	}
	if len(out.Devices) == 0 {
		fmt.Println("no enrolled devices")
		return nil
	}
	fmt.Printf("%-12s %-16s %-9s %-8s %s\n", "DEVICE ID", "NAME", "STATUS", "WORKER", "LAST SEEN")
	for _, d := range out.Devices {
		status := "active"
		if d.Revoked {
			status = "revoked"
		}
		worker := "offline"
		if d.Online {
			worker = "online"
		}
		last := d.LastSeen
		if last == "" {
			last = "-"
		}
		fmt.Printf("%-12s %-16s %-9s %-8s %s\n", d.ID, d.Name, status, worker, last)
	}
	return nil
}

func runDevicesRevoke(args []string) error {
	fs := newFlagSet("devices revoke")
	base := fs.String("url", "", "coordinator HTTP base URL")
	coord := fs.String("coordinator", "", "coordinator base URL (http or ws)")
	token := fs.String("token", env("DEVDOOTH_TOKEN"), "admin token")
	id := fs.String("id", "", "device id to revoke")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return fmt.Errorf("--id is required")
	}
	httpBase, err := resolveBase(*base, *coord)
	if err != nil {
		return err
	}
	body, code, err := apiRequest(http.MethodDelete, httpBase+"/v1/devices/"+*id, *token, nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("coordinator returned %d: %s", code, strings.TrimSpace(string(body)))
	}
	fmt.Printf("revoked %s\n", *id)
	return nil
}

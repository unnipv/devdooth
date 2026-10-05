package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

// defaultMCPCommand is the upstream Playwright MCP server. Devdooth does not
// implement browser tools; it leases a browser and hands it to this server.
// The version is pinned so a lease behaves the same from one run to the next.
const defaultMCPPackage = "@playwright/mcp@0.0.83"

// runMCP acquires a lease, starts a stdio MCP server wired to it, and releases
// the lease when the server exits.
//
// stdout is reserved for the MCP protocol; all Devdooth diagnostics go to
// stderr.
func runMCP(args []string) error {
	fs := newFlagSet("mcp")
	base := fs.String("url", "", "coordinator HTTP base URL")
	coord := fs.String("coordinator", "", "coordinator base URL (http or ws)")
	token := fs.String("token", env("DEVDOOTH_TOKEN"), "API token")
	node := fs.String("node", "", "preferred node name")
	profile := fs.String("profile", "", "worker-local persistent profile name")
	browser := fs.String("browser", "", "requested browser name")
	headful := fs.Bool("headful", false, "request a headful browser")
	ttl := fs.Int("ttl", 3600, "lease TTL in seconds")
	mcpCmd := fs.String("mcp", "", "override the MCP server command (default: npx -y "+defaultMCPPackage+")")
	if err := fs.Parse(args); err != nil {
		return err
	}
	httpBase, err := resolveBase(*base, *coord)
	if err != nil {
		return err
	}

	req := map[string]any{
		"node": *node, "profile": *profile, "browser": *browser,
		"headful": *headful, "ttl_seconds": *ttl,
	}
	payload, _ := json.Marshal(req)
	body, code, err := apiRequest(http.MethodPost, httpBase+"/v1/leases", *token, payload)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("coordinator returned %d: %s", code, strings.TrimSpace(string(body)))
	}
	var lease map[string]any
	if err := json.Unmarshal(body, &lease); err != nil {
		return err
	}
	endpoint, _ := lease["endpoint"].(string)
	leaseID, _ := lease["lease_id"].(string)
	if endpoint == "" || leaseID == "" {
		return fmt.Errorf("coordinator did not return a usable lease: %s", body)
	}

	releaseLease := func() {
		if _, code, err := apiRequest(http.MethodDelete, httpBase+"/v1/leases/"+leaseID, *token, nil); err != nil {
			fmt.Fprintf(os.Stderr, "devdooth: releasing lease %s failed: %v\n", leaseID, err)
		} else if code != 200 {
			fmt.Fprintf(os.Stderr, "devdooth: releasing lease %s returned %d\n", leaseID, code)
		}
	}
	defer releaseLease()

	var name string
	var cmdArgs []string
	if *mcpCmd != "" {
		parts := strings.Fields(*mcpCmd)
		name, cmdArgs = parts[0], parts[1:]
	} else {
		name, cmdArgs = "npx", []string{"-y", defaultMCPPackage}
	}
	cmdArgs = append(cmdArgs, "--cdp-endpoint", endpoint)
	cmdArgs = append(cmdArgs, fs.Args()...)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := exec.CommandContext(ctx, name, cmdArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

	fmt.Fprintf(os.Stderr, "devdooth: lease %s ready on node %v; starting %s\n",
		leaseID, lease["node"], name)
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("mcp server exited: %w", err)
	}
	return nil
}

# AGENTS.md

Instructions for coding agents setting up or working in this repository.

## What it is

Devdooth turns machines you own into a browser pool. One Go binary (`devdooth`)
contains a coordinator, a worker, and a CLI. Workers dial out; each leased
browser's debug port stays on loopback; profiles stay on the worker.

Layout: `cmd/devdooth` (CLI), `internal/coordinator` (control plane + CDP relay),
`internal/worker` (launches browsers), `docs/` (architecture, deployment,
threat model).

## Run it locally

Prerequisites: Go 1.26+ (or a release binary from GitHub Releases) and an
installed Chrome/Chromium. Node/npx is only needed for the MCP launcher.

```bash
go build -o bin/devdooth ./cmd/devdooth

# 1. Coordinator. It prints an admin token on first start; capture it.
bin/devdooth coordinator --addr 127.0.0.1:8080 --store /tmp/devdooth.db > /tmp/devdooth-coordinator.log 2>&1 &
ADMIN=$(grep -o 'DEVDOOTH_ADMIN_TOKEN=.*' /tmp/devdooth-coordinator.log | cut -d= -f2)   # or pass --admin-token yourself

# 2. Enroll this machine and run a worker on it.
TOKEN=$(bin/devdooth enroll-token --url http://127.0.0.1:8080 --token "$ADMIN" --label local 2>/dev/null)
bin/devdooth join --coordinator http://127.0.0.1:8080 --enroll-token "$TOKEN" --name local --data-dir /tmp/devdooth-worker
bin/devdooth worker --coordinator http://127.0.0.1:8080 --data-dir /tmp/devdooth-worker --headful --profiles demo > /tmp/devdooth-worker.log 2>&1 &

# 3. Lease a browser and verify it.
bin/devdooth nodes --url http://127.0.0.1:8080 --token "$ADMIN"
LEASE=$(bin/devdooth lease --url http://127.0.0.1:8080 --token "$ADMIN" --node local --ttl 600 2>/dev/null)
echo "$LEASE"    # contains "endpoint": a ws:// CDP URL

# 4. Release when done.
bin/devdooth release --url http://127.0.0.1:8080 --token "$ADMIN" --lease "$(echo "$LEASE" | grep -o '"lease_id": *"[^"]*"' | cut -d'"' -f4)"
```

Connect a client to the printed `endpoint` with
`playwright.chromium.connect_over_cdp(endpoint)`, or use any CDP/Puppeteer
client. `go test ./internal/...` runs the fast suite; `make e2e` runs the
real-browser tests (needs Chrome).

## Drive it from another agent

The shortest path is the built-in MCP launcher: it leases a browser, starts
Playwright MCP against it, and releases the lease on exit.

```bash
bin/devdooth mcp --url http://127.0.0.1:8080 --token "$ADMIN" --node local
```

Point an MCP client at that command over stdio, or call the HTTP API directly
(`/v1/leases`, `/v1/lease/{id}/cdp?token=...`). `scripts/browse.mjs` is a small
Playwright CLI for driving a lease one action at a time.

## Copy-paste task for an agent

> Set up Devdooth from AGENTS.md on this machine: build it, start a coordinator
> and a worker, lease a browser, open https://example.com, confirm the page
> title, release the lease, and stop the processes you started.

## Rules

- Never print, log, or commit tokens, device identities, or profile contents.
- Keep browser debug ports on loopback; never expose them.
- Devdooth is not an anti-bot, CAPTCHA, or stealth tool. Do not add such work.
- Single owner: do not introduce multi-tenant assumptions.
- Do not commit build output (`bin/`), local stores (`*.db`), or data dirs.

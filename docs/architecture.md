# Architecture

This note records the decisions behind the first Devdooth slice. It is
deliberately short; update it as the design moves.

## The product boundary

Devdooth leases browsers. It does not automate them.

- The coordinator knows about nodes, capabilities, browser slots, profiles,
  sessions, leases, authentication, and tunnels.
- It does not know about pages, selectors, prompts, workflows, or datasets.
- It relays CDP bytes unchanged. It never parses a CDP message.

The value is making a browser on a machine you own reachable through one
authenticated endpoint, without inbound connectivity and without moving profile
state off the machine.

## Topology

```
client ── HTTP lease API ──▶ coordinator ── outbound control WS ──▶ worker
client ── CDP over WS ─────▶ coordinator ── outbound data tunnel ─▶ worker ──▶ Chrome (loopback)
```

Workers always initiate. Nothing on a user's laptop or Pi listens inbound.

## Decisions

### Go for the core, Node only on the client side

The core is networking, subprocess lifecycle, timers, and authentication. Go
gives one static binary for macOS/Linux/Windows and cross-compiles to ARM64 for
the Pi. Browser launching is `exec` plus reading `DevToolsActivePort`; no
Playwright runtime is required on workers.

Playwright is used only on the caller side (the user's agent host), where it
already exists.

### CDP only in V1

Exposing a browser-level CDP endpoint means ordinary Playwright, Puppeteer, and
raw CDP clients work with no Devdooth-specific SDK. The trade-off is that CDP is
Chromium-only and lower fidelity than Playwright's native protocol. That is
acceptable: the alternative adds a per-worker Playwright runtime and version
constraints for functionality most leases do not need.

### One browser process per lease

Each lease gets its own browser process, user-data directory, controller, and
absolute expiry. This costs more memory than packing many contexts into one
browser, but it makes profile ownership simple and the failure radius small. On
a Pi, start with one slot.

### Worker-local profiles

Profiles live at `<data-dir>/profiles/<name>` on the worker. The coordinator
stores only names and ownership. A profile is a hard scheduling constraint, is
used by at most one browser at a time, and is never copied between machines.

### A dedicated relay, not frp/SSH

General reverse tunnels solve connectivity but leave enrollment, lease auth,
browser lifecycle, and profile locking to Devdooth anyway. A dedicated
WebSocket relay is narrower, uses the same TLS deployment, and keeps the
security model in one place. If reliable relay behavior turns out to require a
real tunneling system, revisit this.

### Leases outlive connections

A lease has its own lifetime, separate from the client WebSocket. A dropped
client can reattach within the lease; a lease expires on its own. The worker
enforces the TTL independently, so an orphaned browser does not survive a
coordinator outage.

Stale profile artifacts are removed before launch: a leftover
`DevToolsActivePort` would otherwise be read as the current debug port on the
next lease.

### Bounded, unparsed relay

The relay copies WebSocket frames, preserving type and boundaries. It sets a
32 MiB message cap and write deadlines. A slow consumer is disconnected; it is
never buffered without limit.

## Deliberate omissions

- **No SDKs yet.** The HTTP API and CLI establish the contract first.
- **Single owner, no ACLs.** Device records carry identity, so adding
  `owner_id` later is not a rewrite.
- **No enforced human takeover.** Pausing an opaque CDP stream does not pause an
  in-flight agent. Headful sessions let a human act locally; coordinated
  ownership is future work.
- **No browser downloads.** Workers use installed browsers. This avoids
  packaging and redistribution work before the core is proven.

## Verified

`make smoke` and `go test ./e2e/` prove, on macOS/arm64 with Chrome 154 (and
against Chrome on Linux x64 in CI):

- a worker launches Chrome with a loopback-only debug port;
- a normal Playwright client connects over CDP through the relay and drives a
  page;
- a persistent profile survives a release and re-lease;
- a second lease on a locked profile is rejected;
- `devdooth mcp` hands the leased browser to upstream Playwright MCP.

A Raspberry Pi 5 worker was validated end to end on real hardware (Linux arm64,
Chromium 154): outbound enrollment, capability reporting, a Playwright client on
another host, traffic proven to originate from the Pi, profile persistence,
loopback-only debugging, worker-kill detection, and reconnect identity. An LLM
agent completed a real Amazon search through the leased Pi browser. See
[docs/validation.md](validation.md).

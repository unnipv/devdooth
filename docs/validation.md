# Validation

Evidence that the architecture works on real hardware, not just in tests.

## Raspberry Pi 5 worker (2026-10-05)

**Setup**

| Role | Machine | Details |
|---|---|---|
| Coordinator + client | MacBook (Apple Silicon) | macOS, Chrome 154, `devdooth` from source |
| Worker | Raspberry Pi 5 Model B, 4 GB | Debian 13 (trixie) aarch64, Chromium 154, headless |

The Pi installed `chromium` from Debian, enrolled with a single-use token
(`devdooth join`), and ran `devdooth worker` with no inbound ports. The
coordinator ran on the Mac and was reachable only on the LAN.

**Result: all 14 acceptance items pass.** Item 14 is cooperative pause/resume:
the controller is detached and blocked while a human uses the headful window; an
in-flight command is not undone, and the agent reattaches after resume.

| # | Acceptance item | Result |
|---|---|---|
| 1 | Worker connects outbound | ✅ Pi dialed the Mac coordinator; no inbound port on the Pi |
| 2 | Coordinator sees correct capabilities | ✅ `linux/arm64`, `chromium`, 1 slot, profile `pi-shopping` |
| 3 | Caller requests a session | ✅ `devdooth lease --node pi5` |
| 4 | Correct worker launches the browser | ✅ Chromium started on the Pi |
| 5 | Ordinary Playwright connects remotely | ✅ `connectOverCDP` from the Mac |
| 6 | Browser traffic originates from the worker | ✅ Pi page hit a Mac-hosted server logged as `192.168.68.116` |
| 7 | Persistent profile survives leases | ✅ `localStorage` value survived release and re-lease |
| 8 | Sessions expire and clean up | ✅ explicit release waits for teardown |
| 9 | Killing a worker marks the node unhealthy | ✅ after the fix below, `offline` within 0.5 s |
| 10 | Reconnect restores node identity | ✅ same device id and name after restart |
| 11 | No externally reachable debug port | ✅ Chromium debug socket bound to `127.0.0.1` only |
| 12 | Invalid session credential cannot connect | ✅ covered by the automated e2e suite |
| 13 | An LLM/browser agent completes a real task | ✅ see below |
| 14 | Headful session paused for local human interaction | ✅ `devdooth pause` / `resume`; covered by an automated test |

### Item 6: traffic really came from the Pi

The Mac served a page on its LAN address. After leasing, the Pi's browser was
driven to `http://<mac>:18090/`, and the Mac's HTTP server logged:

```
192.168.68.116 - - [05/Oct/2026 17:33:23] "GET / HTTP/1.1" 200 -
```

`192.168.68.116` is the Pi. The same session reported `navigator.hardwareConcurrency = 4`
and a Linux user agent.

### Item 13: an LLM agent used the Pi browser

An LLM agent was given only the lease endpoint and a small Playwright CLI. It
navigated to Amazon, searched for `raspberry pi 5 8gb`, parsed the result cards,
and reported the top three products with ratings. No CAPTCHA or bot check
appeared; none would have been circumvented (Devdooth is not an anti-bot
product). Nothing was logged in or purchased.

### Bug found by this validation

`SIGTERM` did not stop the worker: the CLI consumes the signal via
`signal.NotifyContext`, but the control-channel read was not cancellation-aware,
so the process (and its node registration) survived until the 90 s read
deadline. On the Pi, `devdooth nodes` still showed `ready` ten seconds after the
kill. Fixed in [#6](https://github.com/unnipv/devdooth/pull/6) with regression
tests for both the connected and mid-handshake cases.

### Reproduce

```bash
# on the coordinator host
devdooth coordinator --addr 0.0.0.0:8080 --store devdooth.db
devdooth enroll-token --url http://127.0.0.1:8080 --token <admin> --label pi5

# on the Pi
curl -fsSL https://raw.githubusercontent.com/unnipv/devdooth/main/scripts/install.sh | sh
devdooth join --coordinator http://<coordinator-host>:8080 --enroll-token <token> --name pi5
devdooth worker --coordinator http://<coordinator-host>:8080 --profiles pi-shopping

# from the client host
devdooth lease --url http://<coordinator-host>:8080 --token <admin> --node pi5
node scripts/browse.mjs '<endpoint>' goto https://example.com
```

## Compatibility

| Platform | Coordinator | Worker | Browser | Status |
|---|---|---|---|---|
| macOS arm64 | ✅ | ✅ | Chrome 154 | tested (local + agent) |
| Linux amd64 | ✅ | ✅ | Chrome (CI runner) | tested in CI |
| Linux arm64 (Pi 5, Debian 13) | ✅ | ✅ | Chromium 154 | tested |
| Windows amd64 | builds | untested | — | builds only; needs a real run |

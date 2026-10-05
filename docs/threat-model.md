# Threat model

Devdooth hands out control of real browsers that may be signed into email,
banking, social, and internal services. That makes a lease a powerful
credential. This document states what Devdooth protects, what it assumes, and
what it explicitly does not.

## Assets

- **Browser sessions and profiles** on worker machines: cookies, tokens,
  localStorage, extensions, and anything the browser can reach.
- **Credentials**: the admin token, device tokens, enrollment tokens, and
  per-lease session tokens.
- **The coordinator** and its metadata store (device names and token hashes).

## Trust model

Devdooth assumes:

- **You trust the coordinator.** It can drive any browser any connected worker
  offers.
- **You trust the automation clients** you give tokens to.
- **Websites are untrusted** and may be hostile.
- **One owner.** There is no multi-tenant isolation.

If any of these is false for you, do not run Devdooth that way. See
[Isolation](#isolation) for how to narrow the blast radius.

## Properties Devdooth provides

- Workers make **outbound connections only**. A worker needs no inbound port,
  and the browser's debug endpoint is bound to loopback on the worker.
- **Admin and worker credentials are separate.** With durable identity enabled,
  the admin token is never accepted as a worker credential. Enrolled device
  tokens are random 256-bit values; the coordinator stores only a hash, and the
  worker keeps its token in `<data-dir>/device.json` with `0600` permissions.
- **Enrollment tokens are single-use and expire.** Redemption is atomic, so a
  replayed token fails. The device name is fixed at enrollment and cannot be
  changed on reconnect.
- **Revocation takes effect immediately**: it drops the device's control
  connection and any active CDP relay.
- **Per-lease credentials** are random, scoped to one lease, and bounded by the
  lease's expiry.
- **The relay is bounded**: a fixed maximum frame size and write deadlines, and
  a stalled peer is disconnected rather than buffered without limit.
- **TTLs are enforced by the worker**, so an abandoned browser is stopped even if
  the coordinator is unreachable.
- **The relay does not parse or log CDP payloads.** Passwords and page content
  are not written to logs.

## Threats and mitigations

| Threat | Mitigation | Residual risk |
|---|---|---|
| Stolen admin token | TLS; keep the token secret; rotate by restarting with a new `--admin-token` | Anyone with it can lease any browser |
| Stolen device token | Stored hashed on the coordinator; revoke the device | A leaked token, or a copy of the worker's `device.json`, impersonates that worker until revoked |
| Stolen enrollment token | Single-use, short TTL | A leaked unused token can enroll one device |
| Replayed enrollment | Atomic single-use redemption | None once used |
| Unauthorized profile access | Profiles require an explicit request and are node-scoped; access is only via a valid lease | A lease can read whatever that profile is logged into |
| Malicious website | Normal browser isolation | The page can attempt phishing, drive-bys, or data exfiltration through its own origin |
| SSRF / local network reach | Browsers reach what their network reaches | A CDP client can direct the browser at local-network services |
| Debug port exposure | Bound to loopback; no inbound worker port | A local process on the worker can reach it |
| Coordinator DoS | Bounded frames, write deadlines, capacity limits | Enough valid leases can exhaust a worker's slots |
| Abandoned session | Worker-enforced TTL and explicit release | Cost is bounded by the TTL |
| Log leakage | No payload logging | Development and credential-recovery paths deliberately print a token once |

## What Devdooth does not defend against

- **A compromised coordinator.** Treat the coordinator host as fully trusted.
- **An authorized CDP client.** Raw CDP can read local files and reach
  local-network services within the browser's privileges. CDP filtering is not a
  security boundary.
- **Multi-tenant abuse.** There is no per-user isolation, quota, or ACL.
- **Detection or evasion.** Devdooth is not a stealth, fingerprint-spoofing, or
  CAPTCHA-bypass tool, and does not pretend to be.

## Isolation

To reduce the blast radius of a compromised lease or worker:

- Run the coordinator on a dedicated host and terminate TLS at a reverse proxy.
- Run workers under a **separate OS account, container, or VM** that holds only
  the profiles and network access you are willing to lend.
- Expose only the profiles you intend to share, and start with read-mostly ones.
- Prefer **short lease TTLs** and revoke devices you no longer use.
- Restrict the worker's network egress where it makes sense. Devdooth does not
  control what the browser can reach.

## Reporting

Report a vulnerability through GitHub's private "Report a vulnerability" flow on
the repository rather than a public issue.

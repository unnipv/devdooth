# Deployment

How to run Devdooth for real: a coordinator behind TLS, and workers on the
machines that lend their browsers.

## Coordinator

The coordinator holds device metadata and relays CDP. Put it on a small host
with a stable address. Workers connect outbound, so only the coordinator needs
an inbound port.

```bash
devdooth coordinator \
  --addr 127.0.0.1:8080 \
  --store /var/lib/devdooth/devdooth.db \
  --admin-token "$(cat /etc/devdooth/admin-token)" \
  --public-url https://devdooth.example.com
```

- `--addr 127.0.0.1:8080` keeps it on loopback; the reverse proxy faces the
  internet.
- `--store` is a single SQLite file (device identities and enrollment tokens).
- `--public-url` is the address callers use. It accepts an `https://` or
  `wss://` URL and is normalised to `wss://` for lease endpoints.
- If `--admin-token` is omitted, one is generated and printed once.

### TLS with Caddy

```
devdooth.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

Caddy gets and renews certificates automatically and proxies WebSockets without
extra configuration.

### TLS with nginx

```nginx
server {
    listen 443 ssl;
    server_name devdooth.example.com;

    ssl_certificate     /etc/letsencrypt/live/devdooth.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/devdooth.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_read_timeout 1h;
    }
}
```

`proxy_read_timeout` must be generous: a lease holds a long-lived WebSocket.

### systemd unit

```ini
[Unit]
Description=Devdooth coordinator
After=network-online.target

[Service]
ExecStart=/usr/local/bin/devdooth coordinator --addr 127.0.0.1:8080 --store /var/lib/devdooth/devdooth.db --public-url https://devdooth.example.com
EnvironmentFile=/etc/devdooth/coordinator.env
StateDirectory=devdooth
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

`/etc/devdooth/coordinator.env` holds `DEVDOOTH_ADMIN_TOKEN=...` with `0600`
permissions. `GET /healthz` is available for a load balancer or uptime check.

## Workers

Install once, enroll, and run the worker on each machine that should lend a
browser. The worker needs no inbound port.

```bash
# enroll (once): stores a device identity under ~/.devdooth
devdooth join --coordinator https://devdooth.example.com \
  --enroll-token "<token from the coordinator>" --name macbook

# run the worker
devdooth worker --coordinator https://devdooth.example.com \
  --profiles shopping,work
```

- **Headful** machines (a desktop) add `--headful` so the browser is visible for
  human take-over.
- **Containers / locked-down CI** may need `--no-sandbox`.
- Give each worker a distinct `--data-dir` if you run more than one per host.

### systemd unit

```ini
[Unit]
Description=Devdooth worker
After=network-online.target

[Service]
ExecStart=/usr/local/bin/devdooth worker --coordinator https://devdooth.example.com --profiles shopping,work
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
```

### Enroll and revoke

Mint a token per machine and revoke when a machine is retired:

```bash
devdooth enroll-token --url https://devdooth.example.com --token "$ADMIN" --label macbook
devdooth devices      --url https://devdooth.example.com --token "$ADMIN"
devdooth devices revoke --url https://devdooth.example.com --token "$ADMIN" --id dev_...
```

## Clients

Callers reach the coordinator over HTTPS and connect to the returned `wss://`
endpoint with any CDP client:

```bash
devdooth lease --url https://devdooth.example.com --token "$ADMIN" --profile shopping --headful
```

```python
browser = await playwright.chromium.connect_over_cdp(endpoint)
```

For an LLM agent, `devdooth mcp` leases a browser and supervises Playwright MCP,
so an MCP client can drive it without extra wiring.

## Checklist

- [ ] Coordinator on a dedicated host, bound to loopback, behind TLS.
- [ ] Admin token stored with `0600`; workers use enrolled device identities.
- [ ] Only the coordinator exposes an inbound port.
- [ ] Workers run under an account that holds only the profiles you intend to lend.
- [ ] Device tokens revoked for machines no longer in service.

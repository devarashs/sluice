# sluice

A gated channel for TCP traffic. One Go binary that moves connections between
hosts three ways:

- **`sluice forward`** moves traffic from a local port to a remote address.
- **`sluice tls entry` / `sluice tls receiver`** form an encrypted hop between
  two hosts, with certificate pinning.
- **`sluice reverse server` / `sluice reverse client`** expose services behind
  NAT or a firewall through a public host, over TLS, multiplexed.

Built for production use at high connection counts. The primary target is
Ubuntu and Debian on x86_64; arm64 binaries are cross-compiled.

Every mode shares one configuration vocabulary, structured logging, a
Prometheus and pprof admin endpoint, a global connection cap and a bounded
per-client rate limit, and a graceful drain on shutdown.

## Install

Download a binary for your platform from the
[releases page](https://github.com/devarashs/sluice/releases) and verify it
against the published `checksums.txt`:

```bash
sha256sum -c checksums.txt --ignore-missing
chmod +x sluice-linux-amd64
sudo install sluice-linux-amd64 /usr/local/bin/sluice
sluice version
```

Or build from source with Go 1.27 or newer:

```bash
git clone https://github.com/devarashs/sluice.git
cd sluice
make build        # produces ./bin/sluice
```

Every mode reads a JSON config: `sluice <mode> -config path.json`. Example
configs for all of them are in [`examples/`](examples). Run any command with
`-h` for its flags, or `sluice help` for the list of modes.

## `sluice forward`

A plain TCP forwarder. Everything arriving on `listenAddr` is dialled through
to `targetAddr`. On Linux this is two kernel sockets joined by `splice`, with
no user-space copy.

```json
{
  "listenAddr": "0.0.0.0:8080",
  "targetAddr": "10.0.0.5:80",
  "limits": { "maxConnections": 200000, "perClient": { "connectionsPerSecond": 50 } },
  "admin": { "listenAddr": "127.0.0.1:9800" }
}
```

```bash
sluice forward -config forward.json
```

## `sluice tls` — an encrypted hop

Two hosts: the **receiver** runs next to the target and terminates TLS; the
**entry** runs next to the clients and originates it. Trust is a pin, not a
certificate authority. The receiver owns a certificate, generates one on first
start if it has none, and the entry holds a copy of that exact certificate and
accepts nothing else. There is no hostname check and no expiry to forget.

On the receiver (the target side):

```json
{
  "listenAddr": "0.0.0.0:9443",
  "targetAddr": "127.0.0.1:10002",
  "certFile": "/etc/sluice/receiver.crt",
  "keyFile": "/etc/sluice/receiver.key"
}
```

```bash
sluice tls receiver -config receiver.json
# On first start it logs the certificate's fingerprint and writes receiver.crt.
```

Copy `receiver.crt` to the entry host, then on the entry (the client side):

```json
{
  "listenAddr": "127.0.0.1:8080",
  "receiverAddr": "203.0.113.10:9443",
  "pinnedCertFile": "/etc/sluice/receiver.crt"
}
```

```bash
sluice tls entry -config entry.json
```

Clients connect to the entry's `listenAddr` in the clear; the traffic crosses
the network encrypted and emerges at the receiver's `targetAddr`.

To connect without a pin during testing, set `"insecureSkipVerify": true` on
the entry instead of `pinnedCertFile`. It is logged loudly and leaves the hop
open to interception; do not use it in production.

## `sluice reverse` — reach a service behind NAT

The **server** runs on a public host. The **client** runs beside the private
services and dials out to the server, so nothing needs to be reachable at the
client. The client tells the server which public ports to open and where each
one leads locally; the server carries every user connection back over one
multiplexed, pinned TLS session.

On the public server:

```json
{
  "tunnelListenAddr": "0.0.0.0:9000",
  "token": "a-long-random-shared-secret",
  "certFile": "/etc/sluice/reverse-server.crt",
  "keyFile": "/etc/sluice/reverse-server.key"
}
```

```bash
sluice reverse server -config reverse-server.json
# Logs the certificate fingerprint and writes reverse-server.crt on first start.
```

Copy `reverse-server.crt` to the client host, then beside the private services:

```json
{
  "serverAddr": "203.0.113.20:9000",
  "token": "a-long-random-shared-secret",
  "pinnedCertFile": "/etc/sluice/reverse-server.crt",
  "bindings": [
    { "publicAddr": "0.0.0.0:8080", "targetAddr": "127.0.0.1:3000" },
    { "publicAddr": "0.0.0.0:8443", "targetAddr": "127.0.0.1:8443" }
  ]
}
```

```bash
sluice reverse client -config reverse-client.json
```

A user reaching `203.0.113.20:8080` is served by the client's local
`127.0.0.1:3000`, and `:8443` by `127.0.0.1:8443`. The token must match on both
ends. The client reconnects on its own with backoff if the server restarts.
Many independent clients can share one server, each with its own public ports.

## Operating sluice

**Admin endpoints.** Every mode serves, on `admin.listenAddr` (default
`127.0.0.1:9800`, loopback unless you change it):

| Path | What |
| --- | --- |
| `/healthz` | liveness, always `ok` while the process runs |
| `/readyz` | readiness; `503` until the mode is accepting, and again while it drains |
| `/metrics` | Prometheus metrics, all prefixed `sluice_` |
| `/debug/pprof/` | Go profiles |

Binding the admin listener beyond loopback exposes pprof and traffic figures to
the network, and sluice logs a warning when you do.

**Shutdown.** One `SIGINT` or `SIGTERM` starts a graceful drain: the mode stops
accepting, lets connections in flight finish for `listener.drainTimeout`, then
closes the rest. A second signal exits at once.

**Configuration.** Durations are strings with a unit (`"30s"`, `"10m"`); sizes
are strings with a unit (`"64KiB"`) or a plain byte count. Unknown or misspelled
fields are rejected at load with the line and field named, so a typo can never
silently disable a limit. See the files in [`examples/`](examples) for every
field.

**Deployment.** A templated systemd unit is in
[`examples/sluice@.service`](examples/sluice@.service), and tuning for high
connection counts — file-descriptor limits and the kernel settings that matter
— is in [`docs/tuning.md`](docs/tuning.md).

**Capacity.** How many connections a host can hold depends on the host, so
sluice ships the harness to measure it rather than a headline number:
`make bench` for throughput and setup rate, `make capacity` for heap per
connection. The method and how to size from it are in
[`docs/capacity.md`](docs/capacity.md).

## Status

Pre-release. Progress and the design decisions behind it are tracked in
[ROADMAP.md](ROADMAP.md).

## Licence

MIT. See [LICENSE](LICENSE).

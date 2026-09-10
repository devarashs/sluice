# sluice roadmap

This file is the tracker. Items move as the work moves. Nothing is ticked before
it has been run and demonstrated. The item being worked on is marked
`(in progress)`; a blocked item says what blocks it.

## Target

One Go binary that moves TCP connections between hosts through gates: a plain
forwarder, a TLS entry/receiver pair, and a multiplexed reverse tunnel.
Production grade. Sized for millions of concurrent connections per node on
Ubuntu/Debian x86_64.

## Decisions

Recorded so later work does not relitigate them. The rationale for each lives
beside the code it applies to.

- **Layout.** `cmd/sluice` for the binary, `internal/` for everything else.
- **CLI.** Stdlib `flag` behind a small nested dispatcher: `sluice forward`,
  `sluice tls entry`, `sluice reverse server`. No CLI framework. A server-side
  security binary carries as few dependencies as it can.
- **Config.** JSON via `-config`. `listenAddr` is where a mode listens,
  `targetAddr` is where it dials. Durations are strings such as `"30s"`, sizes
  are strings such as `"64KiB"`. Shared blocks: `limits`, `admin`, `log`,
  `process`.
- **Logging.** Stdlib `log/slog`. Per-connection events at debug level only.
  At millions of connections, per-connection logging is the bottleneck.
- **Metrics.** Prometheus through `client_golang` on an admin listener bound to
  localhost by default, with `pprof` beside it. Operators cannot run millions of
  connections without runtime metrics.
- **Relay.** TCP-to-TCP copies go through `io.Copy` on raw `*net.TCPConn` so
  Linux uses `splice(2)` and holds no user-space buffer. Every other path uses
  small pooled buffers. Per-connection memory is the number that sets capacity.
- **Listeners.** `SO_REUSEPORT` with one acceptor per CPU on Linux, one acceptor
  elsewhere.
- **Limits.** Global concurrency cap as a channel semaphore, which costs no
  memory per slot and lets the accept loop wait for a slot instead of
  accepting and closing. Per-client rate limiting with `x/time/rate` buckets
  held in a bounded LRU, so memory cannot grow with the number of attacking
  addresses. A client is one IPv4 address or one IPv6 /64.
- **Process.** Go 1.19 and later already raise the open-file soft limit to the
  hard limit, so sluice reads the limit, logs it, and warns when it is low. The
  Go memory limit and GC percent are exposed under `process`.
- **Certificates.** Trust is a pin, not a chain: a client holds the exact
  certificate its server presents and accepts nothing else, so hostnames and
  expiry play no part and operators copy one file. Generated certificates are
  ECDSA P-256, valid ten years, keys written 0600 and never overwritten. Both
  ends speak TLS 1.3 only. Skipping verification is only possible through an
  explicit `insecureSkipVerify: true`, logged loudly.
- **Reverse tunnel.** TLS transport with server certificate pinning. A
  pre-shared token compared in constant time inside the TLS session. yamux for
  multiplexing. The client declares its port bindings in the handshake and the
  server binds them per session, which lets many clients share a server. The
  client keeps a pool of sessions so one TCP connection is not the throughput
  ceiling. Reconnect uses capped exponential backoff with jitter.
- **Dependencies.** `hashicorp/yamux`, `prometheus/client_golang`,
  `golang.org/x/sys`, `golang.org/x/time`, `hashicorp/golang-lru/v2`. Nothing
  else without a reason written here first.
- **Verification.** Integration tests run on real loopback listeners. CI runs
  them on ubuntu-latest, which is the Linux verification. arm64 binaries are
  cross-compiled and unverified.
- **Git.** One branch per item, small commits, a pull request with green CI
  before merge.

## Phase 1: Foundation

- [x] S1 Repo skeleton and CI gate. `sluice version` and `sluice help` run.
      gofmt, vet, race tests, build, and govulncheck pass in CI on every PR.
- [x] S2 Process, logging, metrics, and admin packages. Open-file limit read
      and warned about, memory limit and GC percent from config, slog setup
      from config, a Prometheus registry with the runtime collectors, and an
      admin listener serving `/healthz`, `/readyz`, `/metrics`, and
      `/debug/pprof/`. Tests prove the endpoints answer, the memory limit is
      applied, and log levels filter.
- [x] S3 Relay package. Bidirectional copy with the splice fast path, pooled
      buffers, idle timeout, half-close propagation, byte counters. Tests cover
      the idle timeout firing, half-close, and both directions closing cleanly.
- [x] S4 Config package. JSON loading, duration and size types, address
      validation, defaults, field-named errors. Tests cover malformed input,
      unknown fields, and every default. Taken ahead of S2 and S3 because
      every package's Config struct needs its value types.
- [x] S5 Listener package. Multi-acceptor with `SO_REUSEPORT` on Linux and a
      fallback elsewhere; the accept loop waits on the concurrency cap and
      applies the per-client rate limit before spending a goroutine; TCP
      keepalive; shutdown drains then cancels. Tests prove the connection past
      the cap waits in the backlog, the rate-limited one is closed, and the
      drain timeout is honoured.
- [x] S6 Limits package. Global cap and bounded per-client rate limiter,
      where a client is an IPv4 address or an IPv6 /64. Tests prove bounded
      memory under many distinct clients and refusal at the rate.
- [x] S7 Certificates package. ECDSA generation with SANs, exact-certificate
      pinning with rotation support, explicit insecure flag, half-a-pair
      detection. Tests prove a real TLS 1.3 handshake succeeds only against
      the pinned certificate and that a certificate without its key is
      rejected.

## Phase 2: Modes

- [x] S8 `sluice forward` and the `app` runner every mode shares: logger,
      process settings, metrics registry, admin listener, readiness, signal
      handling with drain. Plain TCP forwarding on the shared packages, with
      the concurrency cap, timeouts, and metrics. Echo test through the
      forwarder, cap enforced, shutdown drains within the configured timeout.
      Demonstrated by hand.
- [x] S9 `sluice tls receiver` and `sluice tls entry`. An encrypted hop with
      certificate pinning, an auto-generated receiver certificate, a handshake
      deadline, and the shared serving tuning. End-to-end TLS test, wrong
      certificate refused unless insecure is explicit, silent and garbage
      clients dropped. Demonstrated by hand.
- [x] S10 Reverse tunnel wire protocol. Versioned handshake with a digested,
      constant-time token, client-declared bindings, and per-binding results,
      plus the per-stream header. Exhaustive tests: bad token, length-varying
      tokens, oversized and truncated frames at every offset, wrong version,
      duplicate bindings, and the count ceiling checked before any body read.
      This is on the unrecoverable list and gets the deepest tests in the repo.
      The handshake deadline is the caller's (S11) to set on the connection.
- [ ] S11 `sluice reverse server` and `sluice reverse client`. TLS, yamux,
      session pool, client-declared bindings, reconnect with backoff, several
      clients at once. End-to-end test from a user through a public port to a
      local echo, two bindings reach the right targets, the client survives a
      server restart. Demonstrated by hand.

## Phase 3: Prove and ship

- [ ] S12 Load-test harness and capacity guide. Measures memory per connection
      and connections per second for each mode and records the numbers in the
      docs so operators can size hosts.
- [ ] S13 Release pipeline. A tag produces linux amd64 and arm64 binaries with a
      checksums file and the version stamped in.
- [ ] S14 Docs and examples. Quick start per mode, example configs, a systemd
      unit with `LimitNOFILE`, and sysctl tuning for high connection counts.
- [ ] S15 Tag v0.1.0.

## Backlog

Agreed out of scope for v0.1.

- Mutual TLS client certificates for the reverse tunnel.
- Per-client tokens with names, for auditing.
- UDP forwarding.
- Windows and macOS builds and verification.
- Config hot reload.

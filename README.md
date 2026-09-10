# sluice

A gated channel for TCP traffic. One Go binary that moves connections between
hosts three ways:

- `sluice forward` moves traffic from a local port to a remote address.
- `sluice tls entry` and `sluice tls receiver` form an encrypted hop between two
  hosts, with certificate pinning.
- `sluice reverse server` and `sluice reverse client` expose services behind NAT
  or a firewall through a public host, over TLS, multiplexed.

Built for production use at millions of concurrent connections per node. The
primary target is Ubuntu and Debian on x86_64.

## Status

Pre-release. Progress and decisions are tracked in [ROADMAP.md](ROADMAP.md).

## Licence

MIT. See [LICENSE](LICENSE).

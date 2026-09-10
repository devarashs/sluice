# Tuning sluice for high connection counts

sluice is written to hold a large number of concurrent connections per host,
but the process can only use headroom the kernel gives it. The defaults on a
stock Ubuntu or Debian box are set for interactive use, not for a relay holding
hundreds of thousands of sockets. This is what to raise and why.

The one figure to keep in mind: **every relayed connection costs two file
descriptors** — one to the client, one to the target or tunnel. A host serving
500,000 concurrent connections needs a bit over 1,000,000 descriptors for the
relay alone.

## File descriptors

The open-file limit is the hard ceiling on concurrency. sluice reads it at
startup, logs it, and warns when it is below 65536. It does not raise it
itself: since Go 1.19 the runtime already lifts the soft limit to the hard
limit, so what is left is raising the *hard* limit, which only the operator
can do.

Under systemd, set it on the unit — the shipped
[`sluice@.service`](../examples/sluice@.service) already carries:

```ini
LimitNOFILE=1048576
```

Keep it at or above twice the `limits.maxConnections` in the config. Outside
systemd, raise it for the service user in `/etc/security/limits.d/sluice.conf`:

```
sluice   soft   nofile   1048576
sluice   hard   nofile   1048576
```

and confirm at runtime with `cat /proc/$(pidof sluice)/limits | grep 'open files'`.

## Kernel settings

These go in `/etc/sysctl.d/99-sluice.conf` and apply with `sysctl --system`.
Treat them as starting points and confirm against your traffic; the right
values depend on connection lifetime and rate.

```ini
# Accept backlog. sluice holds excess clients here while it waits for a
# concurrency slot, so a small backlog drops connections under a burst.
net.core.somaxconn = 65535
net.ipv4.tcp_max_syn_backlog = 65535

# System-wide file handles, above the per-process LimitNOFILE so the box as a
# whole does not run out first.
fs.file-max = 2097152
fs.nr_open = 2097152

# Local source ports for the connections sluice dials outward (forward
# targets, tls receivers, reverse local targets). Without a wide range a busy
# forwarder exhausts ephemeral ports before it exhausts anything else.
net.ipv4.ip_local_port_range = 1024 65535

# Reclaim closed connections sooner so TIME_WAIT does not pin ephemeral ports
# on a host that opens and closes many short connections.
net.ipv4.tcp_tw_reuse = 1
net.ipv4.tcp_fin_timeout = 15

# Larger connection-tracking table if a stateful firewall (nftables/iptables)
# is in the path; an undersized table drops connections with
# "nf_conntrack: table full". Omit if conntrack is disabled.
net.netfilter.nf_conntrack_max = 2097152

# Socket memory. Raise the ceilings so many connections can each hold a
# reasonable buffer; the kernel autotunes within these bounds.
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
net.ipv4.tcp_rmem = 4096 87380 16777216
net.ipv4.tcp_wmem = 4096 65536 16777216
```

## sluice's own knobs

- **`limits.maxConnections`** caps concurrency per listener. Set it from the
  descriptor budget and the memory budget, not higher than the host can hold.
- **`limits.perClient`** rate-limits new connections per client address (an
  IPv4 address or an IPv6 /64), with bounded memory, so one source cannot
  exhaust the cap. Set `connectionsPerSecond` and `burst` to your real client
  behaviour.
- **`process.memoryLimit`** hands the Go runtime a soft heap ceiling; as the
  heap approaches it the collector works harder rather than letting the kernel
  OOM-kill the process. Pair it with `process.gcPercent` if you want fewer
  collections between the current heap and that ceiling.
- **`listener.acceptors`** is the number of accept loops per address; it
  defaults to the CPU count on Linux, where each acceptor has its own
  `SO_REUSEPORT` socket so accepting scales across cores.
- **`bufferSize`** is the per-direction relay buffer for connections that
  cannot use the kernel `splice` path (the TLS and reverse modes). Larger
  buffers raise throughput per connection at the cost of memory per
  connection; the 16KiB default matches a TLS record.

## Measuring on your host

Bring the mode up, drive representative traffic, and watch:

- `sluice_connections_active` and `sluice_connections_accepted_total` on the
  admin `/metrics` for the load actually reached.
- Resident memory (`/proc/<pid>/status` `VmRSS`) divided by
  `sluice_connections_active` for the per-connection cost, which is what turns
  a memory budget into a connection ceiling.
- `go_goroutines` and the GC pause metrics for runtime pressure.
- `ss -s` and the descriptor count in `/proc/<pid>/limits` for headroom before
  the ceilings above.

# Measuring sluice's capacity

sluice is built to hold a large number of connections per host. How many a
given host can actually hold is set by that host: its memory, its CPU, its
kernel settings, and above all whether it is Linux, where the relay's `splice`
fast path holds no user-space buffer at all. So sluice does not publish a
headline number; it ships the harness to measure one where it matters.

This is the method and the tools. Tuning the host first is in
[tuning.md](tuning.md).

## What decides capacity

- **Memory per connection.** The ceiling for idle or low-traffic connections.
  On Linux a forwarded TCP connection is two kernel sockets joined by `splice`,
  so sluice holds only goroutine stacks and bookkeeping per connection. On
  platforms without `splice`, and on the TLS and reverse paths, sluice holds a
  pooled buffer per active copy direction (`bufferSize`, 16KiB by default), so
  per-connection memory is higher.
- **Throughput per connection.** For connections moving data, the limit is CPU
  and copy efficiency. The `splice` path copies in the kernel; the buffered
  path copies through the pooled buffer.
- **Connection setup rate.** New connections per second, bounded by the accept
  path, the concurrency cap, and the host's ephemeral-port range and file
  descriptors.

## The harness

Two tools, both checked in.

**Benchmarks** (`make bench`, or `go test -bench=. -benchmem ./internal/bench/`)
measure relay throughput and connection setup rate. They are Go benchmarks, so
a plain `go test` skips them and the merge gate stays fast. The setup-rate
benchmarks lean hard on ephemeral ports; on an untuned host they skip with a
pointer to [tuning.md](tuning.md) rather than fail.

**`sluice-capacity`** (`make capacity CONNS=50000`, or
`go run ./cmd/sluice-capacity -conns 50000`) opens that many concurrent idle
connections through an in-process forwarder and reports the heap growth per
connection. It is a development and sizing tool; it is not built into the
`sluice` binary or shipped in releases. Raise `ulimit -n` before a large run.

## How to size a host

1. Tune the host per [tuning.md](tuning.md): file descriptors and the sysctl
   settings. Capacity measured on an untuned host is the untuned host's limit,
   not sluice's.
2. Run `sluice-capacity` on that host at a realistic connection count for your
   traffic. Divide your memory budget for the relay by the reported
   per-connection heap to get a memory-bound connection ceiling.
3. Run `make bench` for throughput and setup rate, and for data-heavy traffic
   size by throughput rather than connection count.
4. Bring the real mode up, drive representative traffic, and watch the admin
   `/metrics` (`sluice_connections_active`, the relay byte counters) and
   process RSS, as in [tuning.md](tuning.md). The live numbers under your own
   traffic are the ones to trust.
5. Set `limits.maxConnections` from the lower of the memory-bound and
   descriptor-bound ceilings, leaving headroom.

## Sample run, and why you should not trust it for sizing

Run on the development machine this was built on — **Windows, loopback, so the
buffered path, not Linux `splice`** — purely to show the shape of the output:

```
BenchmarkRelayThroughput-16    922.70 MB/s    1 B/op    0 allocs/op

sluice-capacity -conns 3000
per connection (heap): ~38969 bytes
```

Read those two numbers together with their caveats:

- The ~39KB per connection is the **buffered path on Windows**: two 16KiB
  pooled buffers plus goroutine stacks and bookkeeping, and both ends of every
  connection living in one process. On Linux the `splice` path holds no buffer,
  and in a real deployment the two ends are separate hosts, so the per-host
  figure is markedly lower. Measure on your Linux target; do not carry this
  number over.
- The ~920 MB/s is a single loopback connection on a laptop, bounded by that,
  not by sluice. A tuned server with many connections across cores reaches far
  higher aggregate throughput.

The point of the sample is the method and the output format. The numbers that
should drive a sizing decision are the ones you measure on the host you will
deploy to, under traffic that resembles production.

# cometload: base scenario

Run 2026-10-14 12:09 UTC at commit 438f2cb-dirty.

## Machine

- CPU: Intel(R) Core(TM) Ultra 7 155H, 22 logical cores
- Memory: 30.8 GiB
- OS: linux amd64, kernel 7.1.13-200.fc44.x86_64
- Go: go1.26.7-X:nodwarf5
- Postgres: 17.10 (`make db`, same machine)

Driver, server and Postgres share the machine.

## Load

| Parameter | Value |
|---|---|
| Clients | 1000, over 500 users |
| Subs per client | 5 (2 shared, 3 per-user) |
| Shared groups / rows per result | 50 / 20 |
| Direct writes | 200/s target, 201/s done |
| Calls | 200/s target, 201/s sent |
| Warmup / measured | 10s / 1m0.001s |

## Calls

| Method | Calls | p50 | p99 | max | lag:true |
|---|---|---|---|---|---|
| ping | 2405 | 1.0 ms | 7.2 ms | 11.8 ms | 0.00% |
| shared.bump | 3628 | 55.2 ms | 126.0 ms | 160.6 ms | 0.00% |
| user.add | 2424 | 59.0 ms | 131.5 ms | 155.7 ms | 0.00% |
| user.bump | 3616 | 55.5 ms | 126.0 ms | 164.3 ms | 0.00% |
| **all** | 12073 | **50.0 ms** | **123.2 ms** | 164.3 ms | **0.00%** |

Latency is send to `res` on the client, for calls answered ok.

## Engine

| Metric | Value |
|---|---|
| Ticks | 1283 (21.4/s) |
| Tick duration p50 / p99 / max | 25.2 ms / 57.4 ms / 66.2 ms |
| Runs per tick mean / p99 / max | 17.3 / 29 / 36 |
| Fence wait p50 / p99 | 49.7 ms / 110.2 ms |
| Staleness p50 / p99, other publications | 25.7 ms / 59.2 ms |
| Observers / subscriptions | 1550 / 5000 |
| Max queued bytes / coalescing conns | 26237 / 0 |
| Slow-consumer closes / listener reconnects / dropped clients | 0 / 0 / 0 |
| Subs ended by the server at connect | 0 |

## Resources

| Metric | Value |
|---|---|
| Server RSS idle / loaded | 13.9 MiB / 86.6 MiB |
| Server RSS per 1k connections | 72.7 MiB |
| Connect + initial snapshots | 816ms |
| Postgres CPU | 64% of one core |

RSS per 1k connections is (loaded − idle) / clients × 1000, measured with every sub's snapshot sent. Postgres CPU is the container's cgroup CPU time over the measured window.

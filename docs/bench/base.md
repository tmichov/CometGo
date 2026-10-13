# cometload: base scenario

Run 2026-10-13 10:59 UTC at commit c206ce3.

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
| Warmup / measured | 10s / 1m0s |

## Calls

| Method | Calls | p50 | p99 | max | lag:true |
|---|---|---|---|---|---|
| ping | 2448 | 0.6 ms | 5.7 ms | 8.1 ms | 0.00% |
| shared.bump | 3672 | 36.0 ms | 81.9 ms | 129.8 ms | 0.00% |
| user.add | 2421 | 36.9 ms | 88.3 ms | 140.4 ms | 0.00% |
| user.bump | 3508 | 37.0 ms | 81.5 ms | 136.3 ms | 0.00% |
| **all** | 12049 | **31.7 ms** | **82.1 ms** | 140.4 ms | **0.00%** |

Latency is send to `res` on the client, for calls answered ok.

## Engine

| Metric | Value |
|---|---|
| Ticks | 1727 (28.8/s) |
| Tick duration p50 / p99 / max | 10.5 ms / 33.0 ms / 42.2 ms |
| Runs per tick mean / p99 / max | 12.8 / 21 / 33 |
| Fence wait p50 / p99 | 31.7 ms / 75.1 ms |
| Observers / subscriptions | 1550 / 5000 |
| Max queued bytes / coalescing conns | 16750 / 0 |
| Slow-consumer closes / listener reconnects / dropped clients | 0 / 0 / 0 |
| Subs ended by the server at connect | 0 |

## Resources

| Metric | Value |
|---|---|
| Server RSS idle / loaded | 14.3 MiB / 94.0 MiB |
| Server RSS per 1k connections | 79.7 MiB |
| Connect + initial snapshots | 668ms |
| Postgres CPU | 43% of one core |

RSS per 1k connections is (loaded − idle) / clients × 1000, measured with every sub's snapshot sent. Postgres CPU is the container's cgroup CPU time over the measured window.

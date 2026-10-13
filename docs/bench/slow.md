# cometload: slow scenario

Run 2026-10-13 11:01 UTC at commit c206ce3.

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
| Slow observer | pg_sleep 500ms, re-run on every shared write |
| Slow fn | pg_sleep 2s, one in flight at all times |
| Warmup / measured | 10s / 1m0.001s |

## Calls

| Method | Calls | p50 | p99 | max | lag:true |
|---|---|---|---|---|---|
| ping | 2416 | 0.5 ms | 9.6 ms | 20.9 ms | 0.00% |
| shared.bump | 3576 | 910.9 ms | 1223.6 ms | 1286.1 ms | 0.00% |
| user.add | 2432 | 914.2 ms | 1240.8 ms | 1295.6 ms | 0.00% |
| user.bump | 3623 | 915.8 ms | 1225.8 ms | 1319.3 ms | 0.00% |
| **all** | 12047 | **835.5 ms** | **1223.6 ms** | 1319.3 ms | **0.00%** |

Latency is send to `res` on the client, for calls answered ok.

Slow fn: 19 calls, p50 3071.2 ms, max 3200.6 ms, 0 failed.

## Engine

| Metric | Value |
|---|---|
| Ticks | 98 (1.6/s) |
| Tick duration p50 / p99 / max | 594.4 ms / 645.3 ms / 645.3 ms |
| Runs per tick mean / p99 / max | 164.5 / 190 / 190 |
| Fence wait p50 / p99 | 908.7 ms / 1217.9 ms |
| Observers / subscriptions | 1551 / 5001 |
| Max queued bytes / coalescing conns | 31491 / 0 |
| Slow-consumer closes / listener reconnects / dropped clients | 0 / 0 / 0 |
| Subs ended by the server at connect | 0 |

## Resources

| Metric | Value |
|---|---|
| Server RSS idle / loaded | 13.9 MiB / 89.9 MiB |
| Server RSS per 1k connections | 76.0 MiB |
| Connect + initial snapshots | 736ms |
| Postgres CPU | 37% of one core |

RSS per 1k connections is (loaded − idle) / clients × 1000, measured with every sub's snapshot sent. Postgres CPU is the container's cgroup CPU time over the measured window.

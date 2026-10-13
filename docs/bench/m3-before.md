# cometload: m3 scenario

Run 2026-10-13 11:44 UTC at commit 78f3372.

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
| Direct writes | 200/s target, 200/s done |
| Calls | 200/s target, 200/s sent |
| Slow observer | pg_sleep 10s, re-run on every shared write |
| Slow publication fn | pg_sleep 100ms per sub, 50 clients subscribing as measuring starts |
| Warmup / measured | 10s / 1m0s |

## Calls

| Method | Calls | p50 | p99 | max | lag:true |
|---|---|---|---|---|---|
| ping | 2375 | 0.4 ms | 1.0 ms | 10.3 ms | 0.00% |
| shared.bump | 3584 | 5003.9 ms | 5013.2 ms | 5042.1 ms | 100.00% |
| user.add | 2422 | 5003.9 ms | 5009.6 ms | 5022.3 ms | 100.00% |
| user.bump | 3644 | 5003.9 ms | 5011.1 ms | 5044.3 ms | 100.00% |
| **all** | 12025 | **5003.7 ms** | **5009.7 ms** | 5044.3 ms | **80.25%** |

Latency is send to `res` on the client, for calls answered ok.

Slow publication subs: 50 of 50 ready, dial to ready p50 25844.9 ms, max 25850.4 ms, 0 failed.

## Engine

| Metric | Value |
|---|---|
| Ticks | 6 (0.1/s) |
| Tick duration p50 / p99 / max | 10432.9 ms / 15514.4 ms / 15514.4 ms |
| Runs per tick mean / p99 / max | 1075.8 / 1425 / 1425 |
| Fence wait p50 / p99 | 5000.4 ms / 5001.1 ms |
| Staleness p50 / p99, other publications | 10444.3 ms / 15514.2 ms |
| Staleness p50 / p99, slow.observer | 10444.4 ms / 15514.1 ms |
| Staleness p50 / p99, slow.pubfn | 10444.5 ms / 15514.2 ms |
| Observers / subscriptions | 1601 / 5051 |
| Max queued bytes / coalescing conns | 793 / 0 |
| Slow-consumer closes / listener reconnects / dropped clients | 0 / 0 / 0 |
| Subs ended by the server at connect | 0 |

## Resources

| Metric | Value |
|---|---|
| Server RSS idle / loaded | 14.0 MiB / 87.2 MiB |
| Server RSS per 1k connections | 73.3 MiB |
| Connect + initial snapshots | 477ms |
| Postgres CPU | 21% of one core |

RSS per 1k connections is (loaded − idle) / clients × 1000, measured with every sub's snapshot sent. Postgres CPU is the container's cgroup CPU time over the measured window.

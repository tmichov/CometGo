# cometload: m3 scenario

Run 2026-10-14 12:10 UTC at commit 438f2cb-dirty.

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
| Slow observer | pg_sleep 10s, re-run on every shared write |
| Slow publication fn | pg_sleep 100ms per sub, 50 clients subscribing as measuring starts |
| Warmup / measured | 10s / 1m0.001s |

## Calls

| Method | Calls | p50 | p99 | max | lag:true |
|---|---|---|---|---|---|
| ping | 2384 | 0.7 ms | 5.3 ms | 14.5 ms | 0.00% |
| shared.bump | 3606 | 48.8 ms | 119.2 ms | 180.8 ms | 0.00% |
| user.add | 2451 | 50.8 ms | 126.4 ms | 197.4 ms | 0.00% |
| user.bump | 3594 | 48.4 ms | 120.7 ms | 190.5 ms | 0.00% |
| **all** | 12035 | **43.7 ms** | **117.0 ms** | 197.4 ms | **0.00%** |

Latency is send to `res` on the client, for calls answered ok.

Slow publication subs: 50 of 50 ready, dial to ready p50 459.0 ms, max 778.6 ms, 0 failed.

## Engine

| Metric | Value |
|---|---|
| Ticks | 1409 (23.5/s) |
| Tick duration p50 / p99 / max | 20.3 ms / 54.8 ms / 94.1 ms |
| Runs per tick mean / p99 / max | 22.5 / 40 / 54 |
| Fence wait p50 / p99 | 43.3 ms / 110.7 ms |
| Staleness p50 / p99, other publications | 20.9 ms / 59.2 ms |
| Staleness p50 / p99, slow.observer | 19996.9 ms / 20011.4 ms |
| Staleness p50 / p99, slow.pubfn | 20.9 ms / 57.7 ms |
| Observers / subscriptions | 1601 / 5051 |
| Max queued bytes / coalescing conns | 22307 / 0 |
| Slow-consumer closes / listener reconnects / dropped clients | 0 / 0 / 0 |
| Subs ended by the server at connect | 0 |

## Resources

| Metric | Value |
|---|---|
| Server RSS idle / loaded | 13.7 MiB / 86.7 MiB |
| Server RSS per 1k connections | 73.0 MiB |
| Connect + initial snapshots | 1.706s |
| Postgres CPU | 55% of one core |

RSS per 1k connections is (loaded − idle) / clients × 1000, measured with every sub's snapshot sent. Postgres CPU is the container's cgroup CPU time over the measured window.

## M3 exit criterion

The report above is the median m3 run of three. They alternated with three base runs on the same machine, 2026-10-14 12:04-12:11 UTC, and [base.md](base.md) is the median base run of that set. The commit is marked `-dirty` only because of the documentation changes that were uncommitted at the time; the engine code is the same as in 61eb4d1. The criterion is for connections not subscribed to the slow publications, which is every connection that makes calls in this scenario.

| | run 1 | run 2 | run 3 | median | criterion |
|---|---|---|---|---|---|
| base, overall p99 | 152.5 ms | 88.0 ms | 123.2 ms | 123.2 ms | limit 147.8 ms (+20%) |
| m3, overall p99 | 108.3 ms | 123.1 ms | 117.0 ms | **117.0 ms** | met |
| m3, lag:true | 0.00% | 0.00% | 0.00% | **0%** | met |
| m3, p99 stale_seconds, other publications | 49.5 ms | 48.8 ms | 59.2 ms | **49.5 ms** | met (under 5 s) |

The machine was busy with desktop use during these runs (load average about 1 at the start and about 6 at the end), which is why the base p99 is higher than in earlier baselines. An earlier set taken on a quiet machine, on the same engine without the CMTG-80 close fix (commit ecf0d63), gave a base median p99 of 49.6 ms and an m3 median of 56.5 ms (+14%), with lag:true at 0% and staleness p99 around 20 ms. [m3-before.md](m3-before.md) is the same scenario before M3: overall p99 5009.7 ms, with 80% of calls answered with lag:true.

Two later sets on the same machine, at 12:12 and 12:20 UTC, are not counted. The base scenario itself was stalling in them, with overall p99 of 2.9 to 8.5 s, ticks of up to 3.6 s and lag:true on up to 1.3% of calls, so they do not measure the engine. In those sets, some m3 runs also had ticks of about 12 s. These came from the server logging "could not detach a slow observer; the tick waits for it": with the pool saturated, the slow observer could not get a second connection, so the tick waited for its whole run. CMTG-82 tracks making a known-slow query wait for a free connection instead.

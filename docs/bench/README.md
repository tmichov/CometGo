# Load benchmarks

`cmd/cometload` is a load harness for the CometGo server. It opens many WebSocket clients against one server, writes to Postgres and makes calls at fixed rates, and reports call latency and engine health. Its reports in this directory are the baseline that engine changes are measured against.

## Running it

Start the database, then run both scenarios:

```sh
make db
make bench
```

`make bench` rewrites [base.md](base.md) and [slow.md](slow.md). Each run takes about 75 seconds. For a single ad hoc run, call the command directly; every load parameter is a flag (`go run ./cmd/cometload -h`):

```sh
go run ./cmd/cometload -clients 2000 -calls 500 -duration 30s
```

It reads the database from `-db`, or `DATABASE_URL`, defaulting to the `make db` container. It drops and recreates the `cometload` schema on every run, so point it only at a database you can write to freely.

## What it runs

The driver starts the server under test as a separate process (the same binary, with `-serve`) so the server's memory is measured without the clients'. The server registers:

| Name | Kind | What it does |
|---|---|---|
| `shared.group` | publication, `ScopeShared` | Rows of one group of `cometload.shared_items`, with `Match` on the group. |
| `user.bucket` | publication, `ScopeUser` | One bucket of the caller's rows in `cometload.user_items`, with `Match` on owner and bucket. |
| `slow.observer` | publication, `ScopeShared` | Group 0 joined with `pg_sleep`, without `Match`, so every shared write re-runs it. Slow scenario only. |
| `ping` | method | Writes nothing, so it has no fence. |
| `shared.bump`, `user.bump` | methods | Increment one shared row, or one of the caller's rows. |
| `user.add` | method | Inserts a row into one of the caller's buckets and deletes the bucket's oldest, so results keep their size. |
| `slow.fn` | method | `pg_sleep`, then a shared write. Slow scenario only. |

With the defaults, 1,000 clients are spread round-robin over 500 users. Each client subscribes to 2 shared groups (out of 50) and 3 of its own buckets, which comes to 1,550 observers holding 20 rows each. During the run the driver:

- writes 200 rows a second directly to Postgres, outside any call, half shared and half per-user,
- makes 200 calls a second from random clients: 30% `shared.bump`, 30% `user.bump`, 20% `user.add`, 20% `ping`,
- in the slow scenario, also opens one client subscribed to `slow.observer` (500 ms) that calls `slow.fn` (2 s) back to back.

Load runs for a 10 s warmup and then a 60 s measured window. Only calls sent in the window count.

## Reading a report

- **Calls**: latency from sending `call` to receiving `res`, measured on the client, per method and overall, and the share answered with `lag:true` (§6.2 of the [protocol](../protocol.md)). Every method except `ping` waits for its fence, so its latency includes the tick that delivers its write.
- **Engine**: from `Config.Metrics`. Tick duration, ticks per second, observer runs per tick, commit-to-fence wait, and the observer and subscription counts.
- **Resources**: the server process's RSS before any client connects and after every sub has its snapshot; the difference per 1,000 connections; and the Postgres container's CPU time over the measured window, as a percentage of one core (from its cgroup, through `docker exec`).

Driver, server and Postgres share the one machine, so compare numbers only between runs on the same machine. The machine is recorded at the top of each report.

## Comparing an engine change

M3's exit criterion is overall call p99 within the baseline + 20%. To check a change, run `make bench` on the same machine that recorded the baseline, with nothing else heavy running, and compare the **all** row of the Calls table. Commit the new reports with the change only when they become the new baseline.

A single run is noisy on a laptop. Four back-to-back base runs on the baseline machine gave an overall p99 of 82.1, 157.6, 72.8 and 77.9 ms (p50 31.7 to 39.7 ms). One run in four was an outlier at about twice the others. So run the base scenario at least three times on each side of a change and compare the median p99, not one run against [base.md](base.md).

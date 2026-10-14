# Load benchmarks

`cmd/cometload` is a load harness for the CometGo server. It opens many WebSocket clients against one server, writes to Postgres and makes calls at fixed rates, and reports call latency and engine health. Its reports in this directory are the baseline that engine changes are measured against.

## Running it

Start the database, then run every scenario:

```sh
make db
make bench
```

`make bench` rewrites [base.md](base.md), [slow.md](slow.md) and [m3-after.md](m3-after.md). Each run takes about 75 seconds.

[m3-before.md](m3-before.md) records the `m3` scenario on the engine before M3 (see the [M3 design](../design/m3-slow-work.md)) and is no longer rewritten. [m3-after.md](m3-after.md) records it on the engine with M3, with the exit criterion's three-run medians at the end. For a single ad hoc run, call the command directly; every load parameter is a flag (`go run ./cmd/cometload -h`):

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
| `slow.observer` | publication, `ScopeShared` | Group 0 joined with `pg_sleep`, without `Match`, so every shared write re-runs it. Slow and m3 scenarios. |
| `slow.pubfn` | publication, `ScopeShared` | Its `fn` runs `pg_sleep` on the server's pool before returning its query, on every sub. Each sub passes its own `k`, so each binds its own observer; the query is one shared group, with `Match`. m3 scenario only. |
| `ping` | method | Writes nothing, so it has no fence. |
| `shared.bump`, `user.bump` | methods | Increment one shared row, or one of the caller's rows. |
| `user.add` | method | Inserts a row into one of the caller's buckets and deletes the bucket's oldest, so results keep their size. |
| `slow.fn` | method | `pg_sleep`, then a shared write. Slow scenario only. |

With the defaults, 1,000 clients are spread round-robin over 500 users. Each client subscribes to 2 shared groups (out of 50) and 3 of its own buckets, which comes to 1,550 observers holding 20 rows each. During the run the driver:

- writes 200 rows a second directly to Postgres, outside any call, half shared and half per-user,
- makes 200 calls a second from random clients: 30% `shared.bump`, 30% `user.bump`, 20% `user.add`, 20% `ping`,
- in the slow scenario, also opens one client subscribed to `slow.observer` (500 ms) that calls `slow.fn` (2 s) back to back,
- in the m3 scenario, M3's exit-criterion setup: one client subscribed to `slow.observer` at 10 s (`-slow-observer`), and, as the measured window starts, 50 clients (`-slow-pub-subs`) that each subscribe to `slow.pubfn`, whose `fn` takes 100 ms (`-slow-pub-fn`). An explicit `-slow-observer` or `-slow-pub-fn` overrides the scenario's value. The `slow` scenario is unchanged, so its recorded runs stay comparable.

Load runs for a 10 s warmup and then a 60 s measured window. Only calls sent in the window count.

## Reading a report

- **Calls**: latency from sending `call` to receiving `res`, measured on the client, per method and overall, and the share answered with `lag:true` (§6.2 of the [protocol](../protocol.md)). Every method except `ping` waits for its fence, so its latency includes the tick that delivers its write.
- **Engine**: from `Config.Metrics`. Tick duration, ticks per second, observer runs per tick, commit-to-fence wait, and the observer and subscription counts. **Staleness** is `stale_seconds`: per delivered result, the time since the change it reflects was drained. It is reported for every publication except `slow.observer` and `slow.pubfn` together, and, in the slow and m3 scenarios, for each of those two separately.
- **Slow publication subs** (m3): how long each `slow.pubfn` client took from dialing to its sub being ready.
- **Resources**: the server process's RSS before any client connects and after every sub has its snapshot; the difference per 1,000 connections; and the Postgres container's CPU time over the measured window, as a percentage of one core (from its cgroup, through `docker exec`).

Driver, server and Postgres share the one machine, so compare numbers only between runs on the same machine. The machine is recorded at the top of each report.

## Comparing an engine change

M3's exit criterion is overall call p99 within the baseline + 20%. To check a change, run `make bench` on the same machine that recorded the baseline, with nothing else heavy running, and compare the **all** row of the Calls table. Commit the new reports with the change only when they become the new baseline.

A single run is noisy on a laptop. Four back-to-back base runs on the baseline machine gave an overall p99 of 82.1, 157.6, 72.8 and 77.9 ms (p50 31.7 to 39.7 ms). One run in four was an outlier at about twice the others. So run the base scenario at least three times on each side of a change and compare the median p99, not one run against [base.md](base.md).

## Change-feed cost (feedbench)

`cmd/feedbench` measures what the change feed costs writes, for M4 (see the [M4 design](../design/m4-logical-feed.md)). It runs the same transaction, one `UPDATE` of a random row by primary key, from many connections for 20 s per mode, and reports committed transactions per second:

| Mode | What runs |
|---|---|
| `plain` | No feed: the baseline. |
| `triggers` | `InstallTriggers` on the table, with one connection listening on `comet`. |
| `triggers+fence` | The same, and a `pg_notify` fence in every transaction, as a fenced call sends. |
| `logical` | A publication with the table's key column, and a temporary pgoutput slot read every 50 ms. |
| `logical+fence` | The same, and a transactional `pg_logical_emit_message` in every transaction. |

The logical modes need `wal_level=logical`, which `make db` sets (an existing container picks it up on `docker compose up -d`). Run them all with:

```sh
make bench-feed
```

It writes [feedbench.md](feedbench.md) (16 writers) and [feedbench-64.md](feedbench-64.md) (64 writers). The commit lock that NOTIFY takes shows more with more writers. As with cometload, the numbers are only comparable on one machine with nothing else heavy running.


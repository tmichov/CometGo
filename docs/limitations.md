# Known limitations

These are properties of the current engine, not bugs. Several have planned fixes in the [protocol's later scope](protocol.md#12-scope). Check them against your workload before you rely on CometGo.

## One engine loop per node

Each server runs one engine goroutine. It drains change notifications, runs every observer that needs re-running in one `REPEATABLE READ` snapshot, sends the diffs, and then starts the next tick. A tick's steps run one after another:

- **A slow publication query delays its own subscribers, and costs a connection.** A query still running after `SlowObserverAfter` (1 s) is detached: the tick goes on without it, and only connections subscribed to it wait for its result. Each detached query holds a pool connection and keeps its snapshot open until it finishes, so at most `MaxDetached` (2) run that way. Past that limit, or with detaching off (a pool under 5 connections), a query that turns out slow holds the whole tick again until it finishes or hits `QueryTimeout` (30 s). A subscription to an always-slow query lags its siblings on the same connection by that query's run time.
- **A slow publication `fn` delays its own connection, and shares the workers.** `fn` runs for every new subscription, again when a connection's user changes, and on `Revalidate`. Calls run `FnWorkers` (8) at a time, and a tick waits at most `FnBudget` (50 ms) for the ones it starts. A request whose `fn` is still running then waits for a later tick, together with every later request of its connection, and that connection's method results and `authRes` wait with it. Other connections go on, so a slow `fn` costs the node at most `FnBudget` once. But the workers are shared: while many slow calls are queued, a fast `fn` behind them waits its turn, whether it is another connection's subscription or a re-check of every subscription after the listener reconnects ([How a publication runs](server.md#how-a-publication-runs)).
- **Call results wait too.** Methods run in their own goroutines, so a slow method does not hold up the engine. But a call's result is sent only after the tick that brings its writes to the caller's subscriptions. When ticks are slow, results are slow, or arrive with `lag: true` after `FenceTimeout`.

Keep publication queries indexed and cheap, and keep `fn` free of slow I/O ([M3 design](design/m3-slow-work.md)). The `detached_observers` metric shows when slow queries are running detached. The `tick_seconds`, `tick_runs` and `pub_run_seconds` [metrics](server.md#metrics) show when ticks get long and which publication is slow.

## The change feed uses Postgres NOTIFY

Triggers installed by `InstallTriggers` send one `NOTIFY` per write statement. Every method call that writes also sends one for its read-your-writes fence. This has costs:

- **Write throughput.** Postgres serializes the commits of all transactions that issued a `NOTIFY`, on a single database-wide lock, so very write-heavy workloads on the tracked tables can bottleneck on commit.
- **No transaction-mode poolers for the pool.** The server runs `LISTEN` on a dedicated connection opened with the same settings as `Config.DB`. `LISTEN` needs a session that stays on one backend, so it does not work through PgBouncer in transaction mode, or through pooled endpoints such as Supabase's or Neon's transaction poolers. There is no separate DSN for the listener, so point `Config.DB` at a direct connection or a session-mode pooler.
- **Lost notifications are recovered by re-running everything.** If the `LISTEN` connection drops, every observer re-runs after it reconnects (see [InstallTriggers](server.md#installtriggers)). A connection whose network path dies without a reset (a failover that moves the IP, a black hole) is noticed by a liveness check: after 30 s without a notification the listener runs `SELECT 1`, and drops the connection if that takes more than 5 s. Until then, which is up to about 35 s, subscriptions on that node are stale and fenced calls report `lag`. Each reconnect attempt then gives up after 10 s (or the DSN's `connect_timeout`, if set) and is retried with backoff, so the listener is back shortly after the database is reachable again. The listener interrupts its idle wait with a read deadline, so it ignores a custom `BuildContextWatcherHandler` in the pool's connection config.

The logical feed (`Config.Feed = FeedLogical`) avoids all three: no triggers and no `NOTIFY`, so commits do not serialize; `Config.DB` may go through a transaction-mode pooler, with only `ReplicationDSN` direct (tested with PgBouncer 1.24 in transaction mode; pgx prepares statements, so PgBouncer needs `max_prepared_statements` above 0, which 1.21 and later support); and a dropped connection resumes its slot without losing anything. See [server.md](server.md).

## Multiple nodes

Nodes share nothing but Postgres:

- **Database load grows with node count.** Every node keeps its own observers and re-runs them on every relevant change. Ten nodes that each serve subscribers of the same publication run its query ten times per change.
- **Session resume needs sticky routing.** Sessions live in the memory of the node that served them. A reconnect that reaches another node falls back to a full resync ([Session resume](server.md#session-resume)).
- Events, `Revalidate` and method fences do reach every node, through Postgres notifications.

## Re-runs are full queries

When a table changes, the engine re-runs each affected observer's whole query and diffs the result against what it holds. It does not maintain results incrementally. A change to a large or expensive query costs the full query again, however small the change.

[`Query.Match`](server.md#skipping-unaffected-re-runs-querymatch) cuts the number of observers re-run, but only for single-table queries whose rows are exactly the ones where some columns equal fixed values. A query with a join, a range condition or a `LIMIT` cannot declare `Match`, so every write to its tables re-runs it. A statement that changes more than 1,000 rows re-runs every observer of the table, `Match` or not.

## No optimistic UI

Methods are server-authoritative only. The client does not apply a call's effects locally before the server confirms them. The UI updates when the method's writes arrive, which with read-your-writes is when `await call()` resolves. Optimistic UI is specified in the protocol but *planned*.

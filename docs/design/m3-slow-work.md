# M3 design: slow-work isolation

Status: agreed design, not implemented. The wire-visible parts are in the [protocol](../protocol.md) (§3.2, §4.5, §6.1, §6.2, §8.4, marked **[later]**). This page holds the engine design and the rules each implementation task must keep.

**Goal.** A slow observer query or a slow publication `fn` cannot stall the other subscriptions or calls on the node.

**Exit criterion.** The setup is one injected 10 s observer plus a publication `fn` that takes 100 ms per call. For connections not subscribed to the slow publication, in the [load harness](../bench/README.md):

- overall call p99 stays within the [baseline](../bench/base.md) + 20%, comparing the median of 3 runs,
- `lag:true` stays at 0,
- p99 `stale_seconds` stays under 5 s.

## Where the time goes today

Measured with `cmd/cometload` (1,000 clients, 1,550 observers, 200 writes/s, 200 calls/s):

| Scenario | Call p99 | Tick p50 | Fenced calls with `lag:true` |
|---|---|---|---|
| Baseline | 82 ms | 10.5 ms | 0% |
| 500 ms observer | 1.22 s | 594 ms | 0% |
| 10 s observer | 5.01 s (`FenceTimeout`) | 10.5 s | 100% |

Unfenced calls (`ping`) stay under 2 ms in every scenario. Three properties of the engine cause the rest:

1. **Observers run one after another.** `runObservers` runs every observer due in a tick, in turn, in one `REPEATABLE READ` transaction on one connection. A tick lasts as long as all of its queries put together. With the 10 s observer, about 1,000 other observers pile up behind it in each tick.
2. **Fences are released in bulk.** They are released at the end of the tick, which assumes every dirty observer ran in it (step 5 of `tick`). That is why one slow query delays every call on the node, not only the calls of its own subscribers.
3. **`fn` runs on the engine goroutine.** It runs in `handleReq`, via `bind` and `recheck`, one call after another. N new subscriptions that each run a 100 ms `fn` stall the loop for N × 100 ms. The harness does not cover this yet (its `slow.fn` is a slow *method*, which runs on its own goroutine).

## Design

### 1. Per-connection fence release (prerequisite)

The engine keeps, per observer:

- `staleFrom`: the smallest notification number `n` that the observer's last *delivered* result does not reflect. 0 means fresh. An observer with no result yet counts as stale.
- `dirtySince`: while a run is in flight, the smallest `n` that dirtied the observer after that run's drain cutoff. On completion, `staleFrom = dirtySince` and `dirtySince = 0`.
- `resTick`: the tick whose snapshot produced `res`.

Rules:

- **Match candidates.** A candidate the in-snapshot filter finds unaffected becomes fresh as of that snapshot. If the filter fails, it stays stale and dirty, as today.
- **Assembly.** It emits entries, both snapshots and ops, only for observers with `resTick` equal to the current tick. A sub with `needInit` or `needsReset` on any other observer waits. This keeps §6.1.1 once ticks stop running every observer.
- **Fence release.** A fence `f(p, C)` is released when every sub of C has an observer that is not stale at or before `p`, and has no `needInit` or `needsReset`. (Subs are never left unbound across ticks: §2 keeps `fn` within one tick.) Otherwise the fence is carried to the next tick. A fence dropped by `FenceTimeout` also leaves the carry list.
- **`authRes` after a rebind.** It is held under the same condition. A connection's `after` messages form a FIFO and always go out in order.
- **Events.** An event with number `n` for observer O is held until O is fresh past `n`. It then goes only to subs whose snapshot has been sent. Events are delivered before fences are checked, so an emitting call's `res` (fence `p > n`) still follows its events.
- **Revalidation.** It becomes a numbered notification, stamped by the listener under `e.mu` like the others. The ends it produces are applied in the tick that drains it, before fences.
- **Listener reconnect.** The all-dirty notification's `n` becomes every observer's `staleFrom`, and becomes `p` for the fences committed before that LISTEN.
- **Coalescing (§8.1).** Before the snapshot, a connection is *forced* out of coalescing only if one of its fences or `after` messages is expected to be releasable: every stale observer of the connection is in this tick's run list and not in flight. `release()` turns `needsReset` into `needInit` per observer, and only for observers that ran this tick. While any of its subs is still `needsReset`, a connection stays in `e.coalescing` and keeps `c.coalescing` set. This deliberately splits one connection's subs between the two modes, so the "decided once per connection" comment in `hold` changes with it.

**Tests** (with an internal hook that defers chosen observers):

- a fence on C is released while an observer of another connection stays dirty,
- a fence on C is held while C's own observer is dirty from `n ≤ p`, and released by the tick that runs it,
- a carried fence times out to `lag:true` and leaves the carry list,
- an unaffected Match candidate does not hold a fence,
- a sub awaiting init holds the fence,
- the listener all-dirty path,
- an evt for a deferred observer is held until its data, and still precedes its call's `res`,
- an evt is not sent to a sub whose init is unsent,
- `authRes` after a rebind waits for a deferred sub's reset, and two quick rebinds answer in order,
- a forced coalescing connection with a deferred observer stays coalescing for that sub,
- `Revalidate`'s end still precedes the `res` of the method that caused it.

### 2. Concurrent `fn` calls within a tick

In the request phase of a tick, `bind` and `recheck` no longer call `fn` inline. The tick:

1. collects the `fn` calls its drained requests need (sub, rebind, revalidate),
2. runs them concurrently, bounded by `Config.FnWorkers` (default 8), each with the tick's context,
3. waits for all of them,
4. applies the results on the engine goroutine, in request order, exactly as today.

The identity is still read at handle time, and nothing outlives the tick, so sub/unsub/rebind/revalidate ordering, `MaxSubs`, `MaxObservers` and duplicate handling do not change. A tick then waits for its slowest `fn` rather than for every `fn` in turn.

A single very slow `fn` still holds its tick. That is accepted for M3 and documented. Moving `fn` state across ticks (pending subs, identity generations) was considered and rejected: it adds races with rebind, revalidate and resume that the exit criterion does not need.

**Tests:**

- 50 subs whose `fn` sleeps 100 ms are bound in about one `fn` time, not 50,
- a panic or error in one concurrent `fn` ends only its sub,
- an unsub, or a rebind in the same drain, still wins over the sub it follows,
- `Revalidate` with concurrent rechecks ends exactly the rejected subs, before the method's `res`.

### 3. Fair order and the tick budget

- **Order.** Observers due in a tick run oldest `staleFrom` first, instead of in Go map order.
- **Budget.** A tick stops *starting* observer queries once `2 × Config.SlowObserverAfter` has passed since its snapshot. Unstarted observers stay dirty and stale, and go first in the next tick. This needs §1, because their connections' fences carry.
- **Invariant.** An observer is in at most one of {this tick's run list, in flight}. Dirty marking, `awaitsInit` forcing and the candidate filter all skip in-flight observers.

**Tests:**

- with many observers each taking about 200 ms, ticks stay within the budget,
- every observer is eventually run (no starvation under constant writes).

### 4. Detach on an exported snapshot

- **Lane setup.** The tick's connection (the "lane") opens `REPEATABLE READ READ ONLY`, runs the candidate filter, and then runs `pg_export_snapshot()` at top level, outside any savepoint.
- **Watching each query.** Each observer query runs on a goroutine. The engine goroutine waits for it with a timer of `Config.SlowObserverAfter` (default 1 s).
- **When the timer fires,** the running observer is *detached*:
  1. The engine acquires a new connection, then on it runs `BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY`, `SET TRANSACTION SNAPSHOT '<id>'`, `SELECT pg_export_snapshot()` (the new id) and `SET LOCAL statement_timeout`, in that order.
  2. Only then does it let the detached goroutine finish its transaction. The detached goroutine waits on an `imported` channel before it commits or rolls back. An exporter that ends first makes the import fail ("snapshot does not exist"). The re-exported snapshot outlives the original exporter. Both behaviours were checked against Postgres 17.
  3. The tick continues on the new lane. Every result of the tick, the detached one included, still comes from one snapshot.
- **Completion.** It is handed to the engine goroutine as a request and applied before the next tick's assembly:
  - only if `e.observers[key] == obs`,
  - only to subs bound before the detach whose `e.conns[c][s]` is still that sub,
  - in its own tx per connection, through `flush`, so `ended` is recorded for resumable sessions and `hold` is honoured.

  An error ends those subs through the same path. `staleFrom = dirtySince`, and the observer is queued again if it is still dirty or gained subs while in flight.
- **Joiners.** A sub that binds while its observer is in flight waits for the next run, whose snapshot is after its bind. The detached result is never sent to it (read-your-writes, §6.2).
- **Limits.** At most `Config.MaxDetached` detached queries are in flight. The default is `clamp(pool MaxConns − 4, 0, 2)`. 0 disables detaching, with a warning at `Start`. An observer that has detached once is *known slow*: it starts only when a detach slot is free, and is otherwise deferred like an unstarted observer. At the cap, a first-time-slow query blocks its tick as in v1-core.
- **Types.** `e.pgTypes` is read and written from several goroutines once queries run off the engine goroutine, so it gets a mutex. `errTypeLookup` in a detached run fails only that run, which is retried. On the lane it still retries the tick.
- **Snapshot age.** A detached query holds its snapshot's xmin for up to `QueryTimeout`, at most `MaxDetached` at a time. The lane's own transaction now lasts at most the tick budget plus one soft deadline.
- **Metrics.** A new gauge, `detached_observers`.

**Tests:**

- a 2 s observer detaches, and other subs and calls on other connections keep ticking,
- its result arrives in its own tx and is not older than what its subs had,
- a sub that joins in flight gets a snapshot that contains a write committed before it subscribed,
- the import completes before the detached transaction ends (force the detached query to finish right at the deadline),
- an observer deleted or re-created while in flight drops its completion,
- a detached run that errors ends its subs and records `ended` for a resumable session,
- known-slow deferral at the cap,
- `MaxDetached = 0` behaves as v1-core.

### 5. Measurement

- **New metric `stale_seconds`** (a duration, with `Pub`). When an observer's result is delivered, it is the time elapsed since the notification it reflects (`staleFrom`) was drained.
- **Harness changes:**
  - a slow publication `fn` scenario (`-slow-pub-fn`, which runs `pg_sleep` in `fn` for one publication's subs),
  - `stale_seconds` p99 in the report, excluding the slow publication,
  - a combined `-scenario m3` that runs the exit-criterion setup.

## Task order

1. Measurement (§5). No dependencies.
2. Per-connection fence release (§1).
3. Concurrent `fn` calls (§2). Independent of the others.
4. Fair order and tick budget (§3). After §1.
5. Detach on an exported snapshot (§4). After §3.
6. M3 verification. After all of them: run the exit criterion, record the results in `docs/bench/`, and drop the *planned* notes from the docs.

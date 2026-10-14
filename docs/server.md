# Server

```go
import "github.com/tmichov/cometgo"
```

A server has three steps: create an `App`, register publications and methods, then start it and mount it at `/_comet`.

```go
app := cometgo.New(cometgo.Config{DB: pool})
if err := cometgo.InstallTriggers(ctx, pool, "todos"); err != nil { … }

cometgo.Publish(app, "todos.mine", cometgo.PubOptions{Tables: []string{"todos"}}, …)
cometgo.Method(app, "todos.add", …)

if err := app.Start(ctx); err != nil { … }
http.Handle("/_comet", app)
```

Register every publication and method before `Start` and before serving requests. The registries are not safe to change while connections are being served.

## App

| Identifier | Description |
|---|---|
| `New(cfg Config) *App` | Creates an app and applies defaults to `cfg`. Panics if `cfg.DB` is nil or its `MaxConns` is below 2. |
| `(*App) Start(ctx) error` | Checks every publication's `Tables` (see [Table checks at Start](#table-checks-at-start)), then starts the Postgres listener, the tick engine and the [idempotency-key](#idempotency-keys) cleanup in background goroutines; they run until `ctx` is done or `Shutdown` stops them. If the check fails, it returns the error and starts nothing, and `Start` may be called again. Panics if called again after succeeding. Until `Start` succeeds, the app's `ServeHTTP` answers every request with 503 (`not started`); clients retry with backoff. |
| `(*App) Shutdown(ctx) error` | Stops the server gracefully: see [Graceful shutdown](#graceful-shutdown). |
| `(*App) Revalidate(ctx, userID)` | Re-checks a user's live subscriptions, on every node, after a permission change: see [Revoking access](#revoking-access-revalidate). |
| `(*App) ServeHTTP(w, r)` | `App` is an `http.Handler`. It upgrades the request to a CometGo WebSocket. Mount it at `/_comet`, the client's default URL. |
| `Subprotocol` | The WebSocket subprotocol, `"cometgo.1.json"`. A request that does not offer it gets HTTP 426 with `{"supported":["cometgo.1.json"]}`. |

## Config

Only `DB` is required. Zero or negative durations and limits are replaced with their defaults.

| Field | Default | Meaning |
|---|---|---|
| `DB *pgxpool.Pool` | required | Pool used for publication queries and method transactions. The `LISTEN` connection is opened separately with the pool's connection config, so it is not one of the pool's connections and does not count against `MaxConns`. Needs `MaxConns` of at least 2 (`pool_max_conns`): a tick holds one connection while it looks up column types on another. If the pool stays exhausted (for example, handlers hold the other connections), that lookup fails after `QueryTimeout` and the tick retries with backoff, so live updates pause rather than hang. |
| `Authenticate` | nil | Validates bearer tokens. See [Authentication](#authentication). If nil, every token is rejected with `UNAUTHENTICATED`. |
| `AuthenticateRequest` | nil | Resolves a cookie-session identity at WebSocket upgrade. See [Authentication](#authentication). |
| `Build string` | `""` | Identifies the current client build. Sent to clients in `welcome`, where the client exposes it as `comet.serverBuild`. The server does not compare it with anything. |
| `OriginPatterns []string` | none | Extra allowed `Origin` hosts for the WebSocket upgrade, as host patterns (e.g. `"app.example.com"`, `"*.example.com"`). Same-origin requests are always allowed; other origins are refused. |
| `Heartbeat` | 25s | The server pings every interval and closes the connection with 4408 if a ping gets no pong within twice the interval. Also sent to the client, which uses it for its own liveness check. |
| `MinTickInterval` | 20ms | Minimum time between engine ticks. Bursts of changes inside the interval are coalesced into one re-run. |
| `Feed` | `FeedNotify` | Change feed. `FeedNotify` listens for the triggers' notifications (see `InstallTriggers`). `FeedLogical` decodes the write-ahead log through a replication slot per node, from the tables `InstallPublication` publishes; it needs `ReplicationDSN`, PostgreSQL 14 or later and `wal_level = logical` ([protocol §11.1](protocol.md#111-later-logical-replication-feed)). Fences, events and re-checks then travel as logical messages, and no `NOTIFY` is sent. |
| `FeedMigrating` | false | Set on every node while a running app moves between feeds ([Moving to the logical feed](#moving-to-the-logical-feed)): re-checks from `Revalidate` and `RevalidateAfterCommit` are sent both as a `NOTIFY` and as a logical message, and logical nodes also listen for the notify nodes'. Needs `wal_level = logical`. |
| `OrphanSlotAfter` | 10 min | Logical feed only: every node drops the `cometgo_*` replication slots of its own database that have been inactive this long, as left behind by a crashed process (a clean `Shutdown` drops its own). Slots of other databases on the cluster, and active slots, are never touched. PostgreSQL 17 dates inactivity itself (`inactive_since`); before 17, a slot counts as inactive from the first sweep that saw it inactive with its current position. Negative disables the sweep. Until an orphan goes it holds WAL, so set `max_slot_wal_keep_size` on the server to bound that. Creating a slot with `max_replication_slots` full fails with an error naming the `cometgo_*` slots that hold places. |
| `ReplicationDSN` | none | Required with `FeedLogical`: a direct connection to the same database for the replication protocol. Poolers do not carry it. |
| `SlotName` | the random node ID | Logical feed only: names this node's replication slot `cometgo_<SlotName>`. Lowercase letters, digits and underscores, at most 55; `Start` rejects anything else. Set a stable name (a pod or host name) so a restarted process resumes its slot instead of leaving an orphan. `Shutdown` drops the slot; a crashed process leaves it behind. |
| `FenceTimeout` | 5s | How long a method result waits for its writes to reach the caller's subscriptions. After that the result is sent with `lag: true`. |
| `IdempotencyTTL` | 24h | How long the result of a call carrying an [idempotency key](#idempotency-keys) is kept. A retry after that runs the method again. Keep it above an hour: the TypeScript client retries a call for up to an hour after first sending it. |
| `QueryTimeout` | 30s | Hard `statement_timeout` for publication queries. A query that exceeds it ends its subscriptions with `TIMEOUT`. Also bounds the column-type lookup for user-defined types, including the wait for its pool connection. |
| `SlowObserverAfter` | 1s | Soft deadline for one publication query. A query still running after it is detached (see `MaxDetached`): it finishes on the tick's snapshot on its own connection while the tick goes on. A tick also stops starting publication queries once twice this has passed since its snapshot began. The ones it did not start keep their last result, run first in the next tick (oldest change first), and hold back the method results and identity changes of their own connections until they have run. Other connections are not held. |
| `MaxDetached int` | `min(2, pool MaxConns − 4)` | Slow queries that may run detached at once, each holding one pool connection until it finishes (at most `QueryTimeout`) and keeping its snapshot open that long. A query whose last run was detached and took at least `SlowObserverAfter` is *known slow*: it starts only while a slot is free and a pool connection can be had within 100 ms, otherwise waits for a later tick, and is detached as soon as it starts, so it never holds a tick for longer than that. A detached run that finishes faster than `SlowObserverAfter` clears this. At the limit, a query that turns out slow for the first time holds its tick. Negative disables detaching; so does a pool with fewer than 5 connections under the default. `Start` logs a warning when detaching is off. |
| `FnWorkers int` | 8 | Publication `fn` calls that run at once, across ticks: for new subscriptions, identity changes and `Revalidate` re-checks. If `fn` queries the database, each running call holds a pool connection, so keep this below the pool's `MaxConns`. |
| `FnBudget` | 50ms | How long a tick waits for the `fn` calls it starts. A request whose call is still running then waits for a later tick, with every later request of its connection. That connection's method results and `authRes` wait for it, and while an identity change waits, the connection gets no updates (it gets every sub's reset once the change applies). Other connections go on. |
| `MaxSubs int` | 500 | Subscriptions per connection. A subscription over the limit is ended with `RATE_LIMITED`. |
| `MaxInflight int` | 32 | Concurrent method calls per connection. A call over the limit gets `RATE_LIMITED` with `retryAfterMs: 100`. |
| `MaxMsg int` | 1 MiB | Largest message the server accepts from a client. A larger message closes the connection. |
| `MaxRows int` | 10,000 | Row cap per publication result. A larger result ends its subscriptions with `RESULT_TOO_LARGE`. |
| `MaxObservers int` | 0 (no limit) | Live queries (observers) on the whole server. One observer serves every subscription with the same publication, args and scope. A subscription that would create one more is ended with `OVERLOADED` (after the publication's `fn` runs, so its own `FORBIDDEN` or `VALIDATION` comes first). Subscriptions that join an existing observer are not limited. The pool is shared, so one client with many distinct args can fill it for everyone. `MaxSubs` bounds each connection's share, so set `MaxObservers` well above the observers you expect, as a backstop rather than a quota. |
| `MaxConns int` | 0 (no limit) | Open WebSocket connections. Over the limit, the upgrade is refused with HTTP 503 and `Retry-After: 5`. Browsers don't expose that header, so the TypeScript client retries with its normal jittered reconnect backoff. |
| `MaxConnsPerIP int` | 0 (no limit) | Open WebSocket connections per `ClientIP`, refused the same way. Users behind one NAT share the limit. A connection that dropped without the server noticing (e.g. a network blip) keeps its slot until the heartbeat times it out, so a limit of 1 or 2 can refuse that user's own reconnect for up to three times `Heartbeat` (75 s by default). |
| `ClientIP func(*http.Request) string` | host of `r.RemoteAddr`, also used when it returns `""` | The address `MaxConnsPerIP` counts against. Behind a reverse proxy, every connection comes from the proxy, so return the client address it forwards (e.g. from `X-Forwarded-For`, trusting only what your proxy sets), or leave `MaxConnsPerIP` at 0. |
| `ResumeGrace` | 60s | How long a session outlives a lost connection, waiting for the client to resume it ([protocol §7.2](protocol.md#72-session-resume)). Unlike the other durations, a **negative** value disables resume, and clients then resync on every reconnect. See [Session resume](#session-resume). |
| `ResumeBuffer int` | 1 MiB | Bytes of not-yet-acknowledged messages kept per session for replay, at most 4 MiB (half the outbound queue limit, since a resume sends the whole replay at once). A disconnected session that outgrows it ends, and its client resyncs. |
| `Logger *slog.Logger` | `slog.Default()` | Where the framework logs, including the details of `INTERNAL` errors. |
| `Metrics func(Metric)` | nil | Receives engine and connection health measurements. See [Metrics](#metrics). |

`MaxSubs`, `MaxInflight` and `MaxMsg` are also advertised to the client in `welcome.limits`.

## Publications

```go
func Publish[A any](a *App, name string, opts PubOptions, fn func(c *SubCtx, args A) (Query, error))
```

`Publish` registers a live query under `name`. When a client subscribes, the subscription's args are JSON-decoded into `A` (unknown fields are rejected) and `fn` returns the SQL to run. The query's result is sent as the subscription's initial snapshot, and it is re-run and diffed every time one of `opts.Tables` changes.

`Publish` panics if `name` is already registered, if `opts.Tables` is empty, or if it is called after `Start`. Register every publication and method before `Start`.

```go
type projectArgs struct {
	ProjectID string `json:"projectId"`
}

cometgo.Publish(app, "tasks.byProject", cometgo.PubOptions{Tables: []string{"tasks", "members"}},
	func(c *cometgo.SubCtx, a projectArgs) (cometgo.Query, error) {
		if c.Identity == nil {
			return cometgo.Query{}, cometgo.NewError(cometgo.CodeUnauthenticated, "log in first")
		}
		return cometgo.Query{
			SQL: `SELECT t.id, t.title, t.done FROM tasks t
			      JOIN members m ON m.project_id = t.project_id AND m.user_id = $2
			      WHERE t.project_id = $1::int8 ORDER BY t.id`,
			Args: []any{a.ProjectID, c.Identity.UserID},
		}, nil
	})
```

### How a publication runs

Each distinct `(publication, args, scope)` combination has one server-side **observer**, shared by every subscription that matches it. `fn` is called for **every** subscription, with that subscriber's `SubCtx`, so an access check in `fn` applies to each caller. Only the first call's `Query`, the one that created the observer, is used. It is reused for every re-run until the last subscription to the observer goes away, and the `Query` from later calls is discarded. So:

- Everything the query depends on must be in `Query.SQL` and `Query.Args`, and the scope must separate callers whose queries would differ (see [Scope](#scope)).
- `fn` runs on the engine's workers, up to `Config.FnWorkers` at a time, so keep it fast. A tick waits up to `Config.FnBudget` (50 ms) for the calls it starts. A subscription whose `fn` takes longer is set up in a later tick, and until then its connection's later requests, method results and `authRes` wait for it. Other connections are not held, but a slow `fn` occupies a worker, so many slow calls delay every connection's new subscriptions.
- `fn` must be safe for concurrent use: it can run at the same time as other subscriptions' `fn` calls, for this publication and others, on different goroutines. Calls for one connection still take effect in the order the client sent them.
- `fn` is the access check for a subscription when it starts, and when the connection's principal changes (log in, log out, switch user, switch `ScopeKey`, or an expired identity), because every subscription is re-bound then. It is **not** re-run when a token refresh or session resume keeps the same `UserID` and `ScopeKey`, even if `Identity.Data` (a role, say) changed, and not by itself when access changes in the database, so neither ends a live subscription. After such a change, call [`Revalidate`](#revoking-access-revalidate) to re-run it, or put the check in the query ([Authorization](#authorization)).
- `fn` runs once per subscription, and again for each of a connection's subscriptions when its user changes. An access check that queries the database therefore costs one round trip per subscriber, `FnWorkers` at a time, and holds a pool connection while it runs. Keep it cheap or cached, especially for a `ScopeShared` publication with many subscribers.
- Returning an error from `fn` ends that subscription only, with that error. Other subscriptions to the same observer keep streaming. Return a `*cometgo.Error` (e.g. `ErrForbidden`) to choose what the client sees; any other error becomes `INTERNAL`.
- A panic in `fn` is recovered: it ends only that subscription with `INTERNAL` and is logged with its stack trace. The server and every other subscription keep running.
- A panic while encoding `Query.Args` (from a `driver.Valuer` or a pgx encoder) is recovered too, each time the query runs: it ends every subscription on that observer with `INTERNAL` (or `TIMEOUT`, if the panic value is Postgres's statement-timeout error) and is logged with its stack trace. Other observers re-run in the same tick are not affected.
- A panic in the pool's tracer (`ConnConfig.Tracer`) is recovered wherever the framework queries, and logged with its stack trace. While the engine reads its snapshot, the tick is retried with backoff, as when the database is unreachable, and subscriptions keep their last data until a tick succeeds. In a method call (begin, idempotency key, commit), the call fails with `INTERNAL`. On the `LISTEN` connection, the listener reconnects. The connection in use goes back to the pool. Panics in the pool's `BeforeAcquire` or `PrepareConn` hooks are recovered the same way, but pgxpool keeps the connection it was preparing, so each such panic takes one connection out of the pool for good. Don't panic in those hooks. When `Query.Args` panics, pgx never calls the tracer's `TraceQueryEnd` for that query, and the framework cannot do it instead, because the tracer's span lives in the context that `TraceQueryStart` returned to pgx. A span or timer started there is never ended.
- Bad args (wrong types, unknown fields) end the subscription with `BAD_REQUEST`. An unknown publication name ends it with `NOT_FOUND`.

All observers that need re-running in one tick run on one `REPEATABLE READ READ ONLY` snapshot, so every update a client receives comes from one consistent database snapshot. A query still running after `SlowObserverAfter` is *detached*: it finishes on that snapshot on its own pool connection, the tick goes on without it, and its result goes out later in its own update, to the subscriptions it had when it was detached. Each update still comes from one snapshot, but two subscriptions on one connection may sit at different snapshots for a while: one waiting for a slow query keeps its older result while the others move on ([protocol §6.1, §8.4](protocol.md#84-slow-observers)). A call's result on that connection waits for the slow query (or `FenceTimeout`); other connections are not held.

### PubOptions

| Field | Default | Meaning |
|---|---|---|
| `Tables []string` | required | Every table the query reads, as `"table"` (meaning `public.table`) or `"schema.table"`. A change to any of them re-runs the query. Each must have triggers from `InstallTriggers`. Write names exactly as Postgres stores them (normally lowercase): they are compared as strings, so `"Todos"` never matches changes to `todos`. `Start` fails if a name does not resolve or lacks triggers. |
| `Key []string` | `["id"]` | The result columns that uniquely identify a row. They must be present in the `SELECT` list and must not be null. A missing key column fails the query with `INTERNAL`. |
| `Unordered bool` | false | Declares that row order carries no meaning. The server then sends no positions, and the client's row order is arbitrary. Leave it false and use `ORDER BY` when order matters. |
| `Scope Scope` | `ScopeUser` | Which connections may share one observer. See below. |
| `Events bool` | false | Allows [events](#events) to be sent to this publication's subscriptions. |
| `Row any` | nil | A struct value describing one result row, used by `Describe` instead of running the query. See [Typed clients](codegen.md#publications). |

### Scope

```go
const (
	ScopeUser   Scope = iota // default
	ScopeShared
)
```

- **`ScopeUser`** (the default) shares an observer only between connections with the same [`Identity.Scope()`](#authentication): the `UserID`, plus the `ScopeKey` if one is set. Anonymous connections share with each other. This is the safe choice for any query that depends on who is asking, **as long as `UserID` and `ScopeKey` together determine the query**. Nothing else is compared: an identity with an empty `UserID` and no `ScopeKey` shares with anonymous connections, and two identities that differ only in `Data` share one observer. If the query depends on anything else in `Identity`, such as a tenant or a role, put it in `ScopeKey`, or pass it in the subscription args and check it in `fn`.
- **`ScopeShared`** shares one observer between every connection. Only use it when the query does not depend on the caller's identity at all, such as a public feed. `fn` still runs for every subscriber, so it can check access per caller (for example, whether the caller is a member of the team in the args) and return `ErrForbidden`.

> **Security:** under `ScopeShared`, only the first subscriber's `Query` is ever run. If the query reads `c.Identity` (a `WHERE user_id = $1` with the caller's ID, a join on the caller's memberships), every later subscriber with the same args gets the **first subscriber's rows**: one user's data is served to another. A per-caller check in `fn` does not prevent this, because it decides only whether the caller may subscribe, not what the query returns. If the query depends on who is asking, keep the default `ScopeUser`.

### Authorization

Put the access check in the query's SQL. Join the table that grants access (memberships, shares, ACL rows) and list it in `Tables`, as `tasks.byProject` above does with `members`:

- **Revocation is live.** Removing a membership is an ordinary write to a table the query reads, so the query re-runs and the rows the user may no longer see disappear from their result. No extra call is needed, and it works the same on every node and after the server's `LISTEN` connection reconnects, because it goes through the normal change path.
- **Granting is live too.** Adding a membership makes the rows appear.
- **Keep the default `ScopeUser`.** The query reads `c.Identity.UserID`, so it must not be shared between users (see the warning in [Scope](#scope)). If it also depends on a tenant or a role, put that in [`Identity.ScopeKey`](#authentication) so each tenant gets its own observer.
- The client sees an empty or smaller result, not an error. When the client needs an explicit `FORBIDDEN`, check in `fn` as well (below).
- Every write to the membership table re-runs every observer of every publication that lists it in `Tables`. [`Query.Match`](#skipping-unaffected-re-runs-querymatch) can't narrow that, because it applies only to single-table queries.

Check in `fn` instead when the decision can't be expressed in the query, or when it should end the subscription with an error rather than empty it:

- an external authorization service, a role or flag in `Identity.Data`, a subscription the caller may not open at all;
- a `ScopeShared` publication whose query is the same for everyone, gated per caller (for example, membership of the team named in the args).

`fn` runs when a subscription starts and when the connection's user changes, not when the permission later changes. After changing a permission that only `fn` checks, call [`Revalidate`](#revoking-access-revalidate) for the affected user; otherwise their live subscriptions keep streaming until they resubscribe.

Use [`Identity.ScopeKey`](#authentication) when one user acts under several identities whose queries differ, such as one per tenant. It gives each its own `ScopeUser` observer, its own `Emit` target and its own idempotency keys. It is not an access check by itself: the query or `fn` still has to enforce what each tenant may read.

### Query and SubCtx

```go
type Query struct {
	SQL   string
	Args  []any
	Match map[string]any // optional, see below
}

type SubCtx struct {
	Context  context.Context
	Identity *Identity // nil when anonymous
}
```

The query is re-run in full when a table it reads changes, so it must be deterministic for a given snapshot: add `ORDER BY` to any publication that is not `Unordered`. `Args` are passed to pgx as query parameters.

`SubCtx.Context` is the engine's context, not the client's.

#### Skipping unaffected re-runs: `Query.Match`

By default, every write to a table re-runs every observer that reads it. With 1,000 users each subscribed to their own rows of `todos` (`ScopeUser`), one insert costs 1,000 queries. `Match` declares which rows the query returns, so the engine can tell which observers a write can affect and re-run only those:

```go
cometgo.Publish(app, "todos.mine", cometgo.PubOptions{Tables: []string{"todos"}},
	func(c *cometgo.SubCtx, _ struct{}) (cometgo.Query, error) {
		return cometgo.Query{
			SQL:   `SELECT id, title, done FROM todos WHERE owner = $1 ORDER BY id`,
			Args:  []any{c.Identity.UserID},
			Match: map[string]any{"owner": c.Identity.UserID},
		}, nil
	})
```

For each write, the engine reads the changed rows once, by primary key, inside the tick's snapshot. It then re-runs an observer only if a changed row matches its `Match`, or its current result holds one of the changed keys (a delete, or a row that stopped matching). The re-run is the normal full query, so what clients receive is unchanged.

`Match` is a promise about the query, and the engine trusts it. It must describe the query's rows exactly:

- The publication reads one table, and `Key` is that table's primary key, with the same columns in the same order.
- The result is every row of the table where each `Match` column equals its value. There is no `LIMIT`, `OFFSET`, join, subquery or other condition. `ORDER BY` is fine. An empty map means every row.
- Values are compared the way pgx decodes the column:
  - `string` for `text` and `varchar` (with a deterministic collation)
  - Go integer types (`int`, `int8`–`int64`, `uint8`–`uint32`, and `uint`/`uint64` up to `math.MaxInt64`) for `int2`, `int4` and `int8`
  - `bool`
  - `[16]byte` or the usual string form for `uuid`

  Other column types are never compared in Go: `char(n)` (its padding), `citext`, enums, floats, numerics, and `text` with a nondeterministic collation all re-run, as without `Match`.

If the engine can't decide, it re-runs the observer, exactly as without `Match`. That happens for:
- a `nil` value, a value of another Go type, or a column the table lacks
- a primary key that isn't `int2`/`int4`/`int8`, `text` or `uuid`, or a result key column whose type differs from the table's (e.g. `id::text AS id`)
- a statement whose change list the triggers could not send: more than 1,000 rows, or keys that do not fit in one notification (about 7,500 bytes), so only the table is reported
- the listener reconnecting

Notifications that arrive between the start of a tick and its snapshot are folded into that tick's checks: their keys are checked too. One that is still on its way from Postgres when the snapshot is taken can slip through. A transaction's notifications can thus be split across two ticks while the first tick's snapshot already holds the whole transaction. An observer skipped on the first tick then catches up on the next, as observers of other tables already do. A call's result still waits for all of its writes (§6.2).

## Methods

```go
func Method[A, R any](a *App, name string, fn func(c *Call, args A) (R, error))
```

`Method` registers a server function under `name`. The call's args are JSON-decoded into `A` (unknown fields are rejected with `BAD_REQUEST`), and the returned `R` is JSON-encoded as the call's result. `Method` panics if `name` is already registered, or if it is called after `Start`.

```go
type Call struct {
	Context  context.Context
	Identity *Identity // nil when anonymous
	Tx       pgx.Tx
}
```

Every call runs in its own Postgres transaction, `c.Tx`:

- If `fn` returns `nil`, the transaction commits and the client gets the result.
- If `fn` returns an error or panics, the transaction rolls back and the client gets the error (a panic becomes `INTERNAL`).

**Always write through `c.Tx`.** Writes made through it are *fenced*: the framework adds a marker notification as the last statement before commit, and holds the result until the engine has processed every change up to that marker and sent the resulting update to the caller. That is what makes the client's `await call(...)` resolve with the write already visible. If that takes longer than `Config.FenceTimeout`, the result is sent anyway with `lag: true`.

A call whose transaction wrote nothing (Postgres assigned it no transaction ID) skips the fence and resolves as soon as it commits, because it has no effects to wait for. A call that used `EmitAfterCommit` keeps the fence, because that function assigns an ID, so its events still reach the client before the result. A replay of an [idempotency key](#idempotency-keys) also keeps the fence. Because a read-only call doesn't wait for a tick, its result can be newer than what the client's subscriptions show at that moment.

Writes through another connection (the pool passed as `Config.DB`, or any other connection) still update subscriptions, because the triggers see them, but they carry no fence: the call can resolve before its effects reach the client. If the handler wrote nothing through `c.Tx`, the call resolves as soon as it commits, before any tick. A write it made on another connection is then very likely not yet visible to the client.

Other details:

- Calls on one connection run concurrently, up to `Config.MaxInflight`. A client that needs two calls in order should await the first.
- `c.Context` is **not** cancelled when the client disconnects. A call that has started runs to completion and commits or rolls back on its own. If the client resumes its session, it gets the result; otherwise it reports `CONNECTION_LOST`, because it cannot know which happened.
- `c.Context` **is** cancelled when the client cancels the call (`signal` in the TypeScript client), and only then: it stays valid after the call returns, so work a handler starts with it is not cut off. Pass it to every query so a long one stops promptly. A cancel that arrives while the handler is running always rolls the transaction back and the client gets `CANCELED`, even if the handler ignores the context and returns normally. A cancel that arrives once the handler has returned is too late: the call commits and the client gets the real result.
- A client can send a call with `noFence`, which commits without the fence and returns as soon as the transaction commits. A call that writes nothing already skips the fence; `noFence` is for calls that write but don't need to wait for their update.
- An unknown method name returns `NOT_FOUND`.
- A client can send a call with an idempotency key, so it can retry safely after a disconnect. See [Idempotency keys](#idempotency-keys).

### Idempotency keys

A call whose connection dropped before its result arrived has an unknown outcome. A client that marks a call idempotent (`call(m, args, { idempotent: true })`) attaches a key, `ik`, and re-sends the call after reconnecting. The server runs the method at most once per key and user:

- The first call with a key inserts a row into `comet_idempotency` inside the method's own transaction, runs the handler, and stores the encoded result in that row. The row commits together with the handler's writes.
- A later call with the same key from the same `UserID` and `ScopeKey` gets the stored result back, without running the handler. It still commits with a fence (unless sent with `noFence`), like any call.
- A duplicate that arrives while the first is still running waits for it: if the first commits, the duplicate returns its result; if it fails, the duplicate runs the handler itself.
- A failed call (an error, a panic, a rolled-back transaction) stores nothing, so a retry runs again.
- The same key used for a different method returns `BAD_REQUEST`. The args are not compared.
- Keys are scoped per user. A different user, or an anonymous connection, sending the same key is independent. The same user under another `ScopeKey` (another tenant) gets `BAD_REQUEST`: the call is neither run again nor answered with the other tenant's result. This also applies to a key stored before the app started setting `ScopeKey`, for as long as that key is kept (see below). All anonymous connections share one scope: an anonymous caller who knows another's key gets that caller's stored result. Keys must therefore be unguessable, such as the random UUIDs the client generates; never derive one from an id or other predictable value.
- A key must be a non-empty string of at most 128 bytes, or the call gets `BAD_REQUEST`.
- Stored results are deleted after `Config.IdempotencyTTL` (24h by default) by a cleanup that `Start` runs every hour (every TTL if that is shorter, but at most once a minute).

**Turning on `ScopeKey`.** A key stored without a `ScopeKey` belongs to no tenant, so once identities carry one, a retry of that key gets `BAD_REQUEST`. A call made just before the deploy whose connection dropped is retried after it and refused, although its write committed. Clearing `comet_idempotency` instead would run those retries a second time. The TypeScript client gives up retrying a call an hour after first sending it, so the window is that hour. Deploy the change at a quiet time, or accept that calls cut off by the deploy show an error to the user while their writes went through.

The table must exist: call [`InstallIdempotency`](#installidempotency) at startup. Without it, a call carrying a key fails with `INTERNAL`.

## Events

Events are ephemeral messages to a publication's subscribers, for things that do not belong in a table: progress, typing indicators, "job finished". The publication must set `PubOptions.Events`.

```go
func (a *App) Emit(ctx context.Context, pubName string, args any, scope, topic string, data any) error
func (c *Call) EmitAfterCommit(pubName string, args any, scope, topic string, data any) error
```

An event is addressed to one observer: the publication, its args and its scope. It is delivered to every subscription bound to that observer, and the client receives it through `sub.onEvent(topic, fn)`.

- `args` must encode to the same JSON value the client sent when subscribing (key order and whitespace do not matter). The comparison is on JSON, not on the decoded args: a Go struct without `json` tags encodes `ProjectID`, which does not match a client's `projectId`, and fields the client omitted must be omitted too. Tag your args types and use the same type for the publication and `Emit`. `nil` matches a subscription made without args. An event that matches no subscription is dropped without an error.
- `scope` is the subscribers' `Identity.Scope()` for a `ScopeUser` publication (their `UserID` when they have no `ScopeKey`), and must not be empty: anonymous subscribers cannot be targeted. It is ignored for `ScopeShared`.
- `data` is JSON-encoded.

`Emit` sends the event at once. `EmitAfterCommit` sends it from inside a method's transaction: the event is sent only if the method commits, and the client receives it after the update carrying the method's writes and before the method's result. Use it whenever the event refers to data the method writes.

```go
cometgo.Method(app, "jobs.finish", func(c *cometgo.Call, a FinishArgs) (struct{}, error) {
	if _, err := c.Tx.Exec(c.Context, "UPDATE jobs SET done = true WHERE id = $1", a.ID); err != nil {
		return struct{}{}, err
	}
	return struct{}{}, c.EmitAfterCommit("jobs.mine", nil, c.Identity.Scope(), "finished", map[string]any{"id": a.ID})
})
```

Delivery is **at-most-once**:

- An event reaches the subscriptions bound when the server processes it, on connections that are open. Nobody subscribed means it is dropped.
- Events are not stored. A client does not receive events sent while it was disconnected, even if it resumes its session (see [Session resume](#session-resume)); only an event already sent when the connection dropped can arrive with the resume replay. So a burst of events never fills the replay buffer of a disconnected session.
- Several identical events emitted in one transaction are all delivered.
- Only clients that negotiated the `evt` capability receive events; the TypeScript client always does.

Events travel through `pg_notify` on the channel `comet_evt`, so in a multi-node deployment an emit on any node reaches subscribers on every node. A notification is limited in size: an event whose encoding exceeds 7,500 bytes is rejected with `ErrEventTooLarge`, and nothing is sent. Emitting to an unknown publication, or one without `Events`, also returns an error.

## Errors

```go
type Error struct {
	Code         string `json:"code"`
	Msg          string `json:"msg"`
	Details      any    `json:"details,omitempty"`
	RetryAfterMs int64  `json:"retryAfterMs,omitempty"`
}
```

Return a `*cometgo.Error` from a method, a publication or `Authenticate` to control exactly what the client sees. Any other error is reported as `INTERNAL` with `details.ref`, a short id that also appears in the server log next to the real error. The original message never reaches the client.

| Identifier | Description |
|---|---|
| `(*Error) Error() string` | Returns `"CODE: msg"`, so `*Error` satisfies `error`. |
| `NewError(code, msg string) *Error` | Builds an error with a code and message. |
| `ValidationError(fields map[string]string) *Error` | A `VALIDATION` error with `details.fields` set to the per-field messages. |
| `ErrForbidden` | A ready-made `FORBIDDEN` error. |

### Codes

| Constant | Code | Sent by the framework when |
|---|---|---|
| `CodeBadRequest` | `BAD_REQUEST` | args do not decode, sub args exceed 64 KiB, or a sub id is reused with different name/args |
| `CodeValidation` | `VALIDATION` | never; use `ValidationError` |
| `CodeUnauthenticated` | `UNAUTHENTICATED` | a token is rejected, or no `Authenticate` is configured |
| `CodeAuthExpired` | `AUTH_EXPIRED` | `Identity.ExpiresAt` passes without a refresh |
| `CodeForbidden` | `FORBIDDEN` | never; return it from your own access checks |
| `CodeNotFound` | `NOT_FOUND` | unknown publication or method name |
| `CodeConflict` | `CONFLICT` | never |
| `CodeRateLimited` | `RATE_LIMITED` | over `MaxSubs` or `MaxInflight` |
| `CodeTimeout` | `TIMEOUT` | a publication query exceeds `QueryTimeout` |
| `CodeCanceled` | `CANCELED` | The client cancelled the call before it committed; nothing was written. |
| `CodeResultTooLarge` | `RESULT_TOO_LARGE` | a publication result exceeds `MaxRows` |
| `CodeOverloaded` | `OVERLOADED` | a subscription would create a live query over `MaxObservers` |
| `CodeUnavailable` | `UNAVAILABLE` | never |
| `CodeInternal` | `INTERNAL` | any error that is not a `*cometgo.Error` |

Application-specific codes should use the `app.` prefix, e.g. `NewError("app.quota_exceeded", "…")`.

## Authentication

A connection is either anonymous or bound to an `*Identity`:

```go
type Identity struct {
	UserID    string    // identifies the user
	ScopeKey  string    // optional; partitions one user's identities, e.g. by tenant
	User      any       // sent to the client; defaults to {"id": UserID}
	ExpiresAt time.Time // optional; see below
	Data      any       // free-form, server-side only
}
```

Publications and methods see it as `c.Identity` (nil when anonymous). `User` is what the client sees as `comet.user`; `Data` never leaves the server.

`ScopeKey` splits one user into separate identities, for example one per tenant. `(*Identity).Scope()` combines the two: it returns the `UserID` when there is no `ScopeKey`, and otherwise the `UserID` and `ScopeKey` joined with a NUL byte. As long as `UserID`s contain no NUL, one user's scope can never equal another user's. Two identities with the same `UserID` but different `ScopeKey`:

- get separate `ScopeUser` observers ([Scope](#scope));
- are separate `Emit` targets: pass `Scope()` as `Emit`'s `scope` ([Events](#events));
- are different principals on an auth change, so switching tenant rebinds and resets every subscription, like a login;
- cannot [resume](#session-resume) each other's sessions;
- cannot reuse each other's [idempotency keys](#idempotency-keys).

`UserID` and `ScopeKey` must be valid UTF-8 and contain no NUL byte: Postgres `text` cannot store a NUL, and `Scope()` uses NUL as its separator. An identity that breaks this is refused as an invalid login: `Authenticate`'s token is rejected with `UNAUTHENTICATED`, and an `AuthenticateRequest` session is closed with 4401, as if the function had returned an error. Either way the server logs which field was wrong.

`ScopeKey` only partitions a single user; it never makes different users share. To share data across a tenant, use `ScopeShared` with the tenant in the args, and check membership in `fn`.

There are two ways to authenticate.

### Bearer tokens: `Authenticate`

```go
Authenticate func(ctx context.Context, token string) (*Identity, error)
```

Called with the token the client sends when it connects and every time it calls `setToken`.

- Return an identity to accept the token.
- Return a `*cometgo.Error` (typically `UNAUTHENTICATED`) to reject it with that error, or `(nil, nil)` to reject it as `UNAUTHENTICATED`.
- An invalid token never closes the connection. At connect, the connection continues as anonymous and the client is told the token was rejected. On `setToken`, any error (a rejection, or a failure such as a timeout reaching your auth store) leaves the current identity and its subscriptions in place until it expires; only the client's `setToken(null)` logs out.

When the identity changes (login, logout, a different user), every subscription on the connection is re-bound to the new identity and reset in one atomic update. Subscriptions the new identity may not read end with the error their publication returns. Calls already running keep the identity they started with. A refresh that keeps the same `UserID` and `ScopeKey` changes nothing on the connection's subscriptions; a change of `ScopeKey` alone (another tenant) counts as a different identity.

### Cookie sessions: `AuthenticateRequest`

```go
AuthenticateRequest func(r *http.Request) (*Identity, error)
```

Called with the upgrade request, so it can read session cookies. Its result is used only when the client connects without a token.

- Return `(nil, nil)` for an anonymous connection.
- Return an error to refuse the session: if the client connected without a token, the connection is closed with code 4401.

### Expiry: `Identity.ExpiresAt`

If `ExpiresAt` is set, the server tells the client when the identity expires (`welcome.authExp` / `authRes.authExp`, in Unix seconds). When that time passes without a new token, the connection is downgraded to anonymous, every subscription is reset as for a logout, and the server sends an `AUTH_EXPIRED` auth result. The TypeScript client does not surface that code: the app sees `comet.user` become `null` and `onChange` fire, the same as a logout. The TypeScript client refreshes on its own before then when its token source is a function ([client Auth](client.md#auth)); with a string token the app should call `setToken` with a fresh one. The HTTP refresh endpoint is *planned* ([protocol §3.2](protocol.md#32-mid-session-auth)).

### Revoking access: `Revalidate`

A publication's `fn` checks access when a subscription starts and when the connection's user changes, but not afterwards. Removing a user from a team does not, by itself, end a subscription they already hold.

The best fix is to make the permission part of the query (see [Authorization](#authorization)). A revocation is then an ordinary write: the query re-runs, and the rows the user may no longer see disappear from the result.

For a check that can't be expressed in SQL, call `Revalidate` after the change, or `RevalidateAfterCommit` from the method that makes it:

```go
func (a *App) Revalidate(ctx context.Context, userID string) error
func (c *Call) RevalidateAfterCommit(userID string) error
```

It re-runs `fn`, with each session's current identity, for every live subscription of every session whose `Identity.UserID` is `userID`, on every node connected to the same database. That includes sessions whose socket dropped and that are waiting to be [resumed](#session-resume); those get the end in the replay. A subscription whose `fn` now returns an error ends with that error (e.g. `FORBIDDEN`). The user's other subscriptions, and other users' subscriptions to the same observer, carry on untouched, and no query is re-run. A panic in `fn` ends that subscription with `INTERNAL`.

- **Only an error ends a subscription.** A `fn` that responds to the change by returning a narrower `Query` (for example, only the teams the user still belongs to) changes nothing, because the observer keeps the `Query` it was created with. To revoke, `fn` must return an error, such as `ErrForbidden`.
- **Any error ends the subscription, a transient one too.** If `fn` queries the database and that query fails during the re-check, the subscription ends with `INTERNAL` and the client has to subscribe again.
- `Revalidate` sends a Postgres notification on the channel `comet_reval` (the payload is the user ID) and returns. Each node re-checks on its next tick after receiving it, against whatever `fn` reads at that point, so call it after the change has **committed**. It returns an error only if the notification could not be sent, or `ErrUserIDTooLong` for a `userID` of 8,000 bytes or more (too long for a notification). In that case nothing is sent, and `RevalidateAfterCommit` leaves the method's transaction usable.
- Requests for the same user that reach a node before one tick are merged: each subscription is re-checked at most once per tick, however many `Revalidate` calls named its user. A bulk revocation costs one pass over the node's sessions per tick, not one per call.
- Inside a [method](#methods), use `c.RevalidateAfterCommit(userID)`. The notification is part of the method's transaction: it is sent only if the method commits, and the re-check runs once the change is visible. A subscription it ends gets its end before the method's result (unless the call was sent with `noFence`). If the method fails and rolls back, nothing is re-checked. Calling `Revalidate` from the handler instead can run the re-check before the change is visible.
- A node whose `LISTEN` connection is down when the notification is sent misses it. To catch up, the tick after the listener reconnects re-checks **every** live subscription on that node, the same way it re-runs every observer's query for row changes it missed. So the revocation applies once the listener is back, not straight away, and each reconnect costs one `fn` run per subscription. Keep `fn` cheap.
- An empty `userID` does nothing: anonymous sessions have no per-user permissions to revoke.
- A session whose user is changing (an auth request is in progress) is skipped; the rebind that follows re-checks every one of its subscriptions anyway.

## InstallIdempotency

```go
func InstallIdempotency(ctx context.Context, db *pgxpool.Pool) error
```

Creates the `comet_idempotency` table (and an index on `created_at`) that stores results for [idempotency keys](#idempotency-keys), in the `public` schema or whatever the connection's `search_path` resolves first. It is idempotent; run it at startup or from a migration. An app whose clients never send keys does not need it.

```sql
comet_idempotency(ik text, user_id text, method text, result json, created_at timestamptz,
                  PRIMARY KEY (ik, user_id))
```

`result` is `json` rather than `jsonb` so that a retry returns the same bytes as the first call.

## Session resume

A client that loses its connection briefly (a network blip, a laptop waking up) resumes its session instead of starting over. The server keeps the session for `Config.ResumeGrace` after the socket drops: subscriptions stay bound, running calls finish, and everything that would have been sent is kept in a replay buffer of up to `Config.ResumeBuffer` bytes. When the client reconnects in time, it gets exactly the messages it missed: no snapshots are re-sent, and no call result is lost.

The session is not resumed, and the client falls back to a full resync, when:

- the server restarted,
- the grace window passed,
- the buffer overflowed while the client was away,
- the reconnecting client authenticates as a different user, or as the same user with a different `ScopeKey`, than the session's.

A token refresh for the same user and `ScopeKey` is fine. A protocol error, or a deliberate close by the client (code 1000/1001, e.g. a page unload), ends the session for good; a heartbeat timeout or a slow-consumer close leaves it resumable. An anonymous session is resumable by whoever holds its session id, which the client keeps in memory only.

**Multiple nodes.** Sessions live in the memory of the node that served them. Behind a load balancer, route a client's reconnects to the same node (sticky sessions, e.g. by cookie or client IP). Without sticky routing everything still works, but a reconnect that lands on another node resyncs.

Memory cost: each disconnected session holds its subscriptions plus at most `ResumeBuffer` bytes for up to `ResumeGrace`. Connected sessions hold the unacknowledged tail, usually about one second of output. Set `ResumeGrace` negative to turn resume off.

## Graceful shutdown

`App.Shutdown(ctx)` lets a deploy restart the server without failing calls or making every client reconnect at the same moment:

1. Every connection gets a `goaway` with a random delay of 0–5 s. New WebSocket connections are refused with HTTP 503.
2. New calls are refused with `UNAVAILABLE` (with `retryAfterMs`). The TypeScript client does not send them: it holds calls made after `goaway` for the next connection. Calls already running finish, and their results are delivered.
3. Once no call is running, every socket is closed with code 4503 after its queued messages are written.
4. The engine, the Postgres listener and the idempotency cleanup stop.

Each client reconnects after its own delay, so a rolling restart spreads the reconnects (and their resyncs) over five seconds. If `ctx` ends first, `Shutdown` closes the remaining sockets at once and returns `ctx.Err()`. Calls still running then finish on their own, and their clients see `CONNECTION_LOST`.

Pass `Start` a context that outlives the shutdown signal (or `context.Background()`): the engine and the listener must keep running while `Shutdown` drains, or in-flight calls wait out `FenceTimeout` and subscriptions stop updating. Call `Shutdown` before `http.Server.Shutdown`, which does not wait for WebSocket connections. Calling `Shutdown` again, or concurrently, waits for the first one.

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()
if err := app.Start(context.Background()); err != nil { // not ctx: it must outlive the signal
	log.Fatal(err)
}
// ... set up srv, then:
<-ctx.Done()
shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
defer cancel()
app.Shutdown(shutdownCtx)
srv.Shutdown(shutdownCtx)
```

Sessions do not survive the shutdown: a client that comes back resyncs.

## Readiness

`App.Ready()` returns nil while the node serves fresh data, and an error saying why it does not. Wire it to your load balancer's readiness check:

```go
http.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
	if err := app.Ready(); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	}
})
```

It is an error:

- before `Start`, and from `Shutdown` on;
- until the change feed is first up: the `LISTEN` connection, or the logical feed's replication slot. Creating a slot waits for every transaction holding a transaction ID, which can take a while;
- when the feed has been down for more than 10 s.

Shorter outages are reported ready, so a Postgres restart, which every node reconnects from within seconds, does not take every node out of rotation at once. Meanwhile subscriptions lag, and calls wait for their writes up to `FenceTimeout`. The error names the feed and its last connection error.

## Slow clients

A client that reads more slowly than its subscriptions change (a phone on a bad network, a tab in the background) does not make the server buffer every change for it. Once more than 256 KiB of diffs is queued for a connection, the server stops sending it diffs and only notes which subscriptions changed. When the client has caught up below 64 KiB, it gets one transaction that resets those subscriptions to the current data. The client applies a reset as a diff against what it holds, so rows that did not change are not re-rendered. Server memory per connection stays bounded by the size of the results, not by how fast they change.

While the connection is held back:

- A method call's result still arrives after the data it wrote: when its fence is released, the held subscriptions are reset in that tick, ahead of the result.
- Events for the connection are dropped. Events are at-most-once anyway.
- A large snapshot sitting in the queue does not count: a subscription whose result is over 256 KiB keeps getting diffs as long as the client keeps reading.
- If the connection drops, a resumed session gets the messages it missed and then the resets.

A client that makes no progress at all is still disconnected with 4429 SLOW_CONSUMER, when about 8 MiB is queued or a single write is stuck for 10 seconds. That close leaves a resumable session resumable.

## Metrics

Set `Config.Metrics` to receive health measurements, for example to feed Prometheus or OpenTelemetry. CometGo itself has no dependency on either:

```go
type Metric struct {
	Name  string  // one of the Metric* constants below
	Pub   string  // the publication, for MetricPubRun and MetricStale; "" otherwise
	Value float64 // seconds for durations, a number otherwise
}
```

| Name | Kind | Reported |
|---|---|---|
| `MetricTick` (`tick_seconds`) | duration | once per engine tick: how long the tick took |
| `MetricTickRuns` (`tick_runs`) | number | once per tick: how many observer queries it ran |
| `MetricObservers` (`observers`) | gauge | after every tick: live observers |
| `MetricSubscriptions` (`subscriptions`) | gauge | after every tick: bound subscriptions, on all sessions, including detached ones |
| `MetricQueuedBytes` (`queued_bytes`) | gauge | after every tick: bytes waiting to be written, summed over every live session's socket |
| `MetricCoalescing` (`coalescing_conns`) | gauge | after every tick: connections that are [coalescing](#slow-clients) |
| `MetricFenceWait` (`fence_wait_seconds`) | duration | per fenced method call: from its commit until its update was sent (its fence released), or until `FenceTimeout` for a lagged call |
| `MetricFenceLag` (`fence_lag`) | counter | 1 per call answered with `lag: true` after `FenceTimeout` |
| `MetricSlowClose` (`slow_closes`) | counter | 1 per socket closed as a slow consumer: its queue went over 8 MiB, or a write timed out. A write that fails because the peer went away also closes with 4429, but is not counted |
| `MetricListenReconnect` (`listen_reconnects`) | counter | 1 per failed `LISTEN` attempt: each drop, and each failed reconnect during an outage (retried with backoff up to 10 s) |
| `MetricPubRun` (`pub_run_seconds`) | duration | per observer query run, with `Pub` set, reported once the tick's snapshot commits, or for a detached query once its result is applied |
| `MetricDetached` (`detached_observers`) | gauge | after every tick: queries running detached from their tick (`MaxDetached`) |
| `MetricStale` (`stale_seconds`) | duration | per observer result delivered after a change, with `Pub` set: from the tick that drained the first notification the observer's old result did not reflect, until its new result was enqueued to its subscribers (or held for a [coalescing](#slow-clients) connection). A run only to snapshot a new subscription, or a Match candidate the change cannot affect, reports nothing |

Gauges are the current value, so map them to a gauge and set it; a counter is reported as `1` per occurrence; a duration is one observation, for a histogram. A tick happens only when there is work (a change, a request, a timer), so an idle server reports no gauges until something happens.

The hook is called synchronously from the engine goroutine and from connection goroutines, so it must be fast and safe for concurrent use. Do the aggregation there, such as incrementing a counter or recording into a histogram, and never block. A panic in the hook is recovered and logged. When `Metrics` is nil, each measurement point costs one nil check, and the gauges are not computed. When it is set, computing the gauges visits every session after each tick.

## InstallTriggers

```go
func InstallTriggers(ctx context.Context, db *pgxpool.Pool, tables ...string) error
```

Installs the change-feed triggers that tell CometGo which rows changed. Run it at startup or from a migration, for every table named in any publication's `Tables`. It is idempotent and installs all the given tables in one transaction.

Requirements:

- Each table must already exist and must have a primary key.
- Tables are named `"table"` (meaning `public.table`) or `"schema.table"`.
- Postgres 11 or newer (the triggers use `EXECUTE FUNCTION`; the protocol spec's "PG 10+" is the minimum for transition tables, not for this code).

Per table it creates statement-level `AFTER INSERT`, `AFTER UPDATE`, `AFTER DELETE` and `AFTER TRUNCATE` triggers named `cometgo_ins`, `cometgo_upd`, `cometgo_del` and `cometgo_trunc`, and their functions in the table's schema. Each statement sends one notification on the channel `comet` listing the changed primary keys, or a "whole table changed" marker when more than 1,000 rows changed or the keys do not fit in one notification. Every notification for a table re-runs the observers that read it, except observers whose query declares [`Match`](#skipping-unaffected-re-runs-querymatch), which re-run only if the changed rows can affect them.

### UninstallTriggers

```go
func UninstallTriggers(ctx context.Context, db *pgxpool.Pool, tables ...string) error
```

Removes the `cometgo_` triggers from each table and drops their functions, in one transaction. It is idempotent. It finds the functions through the triggers, so it also cleans up a table that was renamed after `InstallTriggers`. Run it only once every node uses the logical feed: a node on the notify feed refuses to start without the triggers (below), so a rollback after `UninstallTriggers` fails at `Start` instead of serving stale subscriptions.

### Table checks at Start

A publication whose table is never notified loads once and then never updates. To catch this early, `Start` checks every distinct table in every publication's `Tables` and returns an error, without starting anything, if:

- no table has exactly that name (`schema.table`, compared as Postgres stores it). This covers a misspelled name, a name in the wrong case (`"Todos"` for `todos`), and an unqualified name for a table outside `public`, which is never `public.table` even when `search_path` would find it. When Postgres can resolve what the name most likely meant, the error suggests it.
- the table lacks a `cometgo_ins`, `cometgo_upd`, `cometgo_del` or `cometgo_trunc` trigger that fires normally (disabled and replica-only triggers count as missing), or its triggers were installed under another name and the table was renamed or moved since, so they would notify under the old name. The error names the triggers; run `InstallTriggers` for the table.

All failing tables are reported in one error, which `Start` also logs. Because of this check, every table must exist and have its triggers before `Start`, so run migrations and `InstallTriggers` first. The check also needs the database: if it is unreachable, `Start` fails within `QueryTimeout` instead of starting and reconnecting in the background. Retry `Start`, or exit and let your supervisor restart the process.

With `Config.Feed` set to `FeedLogical`, the trigger check is replaced by publication checks (see [InstallPublication](#installpublication)): the publication `cometgo` must exist and publish inserts, updates, deletes and truncates via the partition root, and each table must be in it under its own name. Each table, or each partition of a partitioned table, must have `REPLICA IDENTITY DEFAULT` with a primary key, or `FULL`. Under a key-only column list, the list must still contain every replica-identity column. Each error names the table, or the partition, at fault.

The check runs once, at `Start`. Dropping or disabling the triggers, or dropping the table, while the server runs is not detected, and the affected subscriptions silently stop updating.

If the server's `LISTEN` connection drops, notifications sent meanwhile are lost, so on reconnect the server re-runs every observer. It reconnects after 500 ms, doubling the wait after each attempt that fails or drops soon, up to 2 s. Calls committed while it is down wait for the reconnect, so the cap keeps them under the default `FenceTimeout`. After a connection that stayed up for 30 s, the wait starts again at 500 ms. See [protocol §11](protocol.md#11-postgres-change-feed-server-internal-v1) for the payload format.

## InstallPublication

```go
func InstallPublication(ctx context.Context, db *pgxpool.Pool, tables ...string) error
func InstallPublicationKeysOnly(ctx context.Context, db *pgxpool.Pool, tables ...string) error
```

The counterpart of `InstallTriggers` for the logical feed (`FeedLogical`). It adds each table to the publication `cometgo`, creating the publication if it is missing. The publication publishes inserts, updates, deletes and truncates, with `publish_via_partition_root`, so the changes of a partitioned table are reported under the partitioned table's own name, the name `Tables` uses. It is idempotent and runs in one transaction. Running it while the triggers are still installed is fine: the notify feed ignores the publication.

`InstallPublication` publishes whole rows. `InstallPublicationKeysOnly` limits each table's column list to its primary key, which keeps the change stream small for wide tables. It needs PostgreSQL 15 or newer and `REPLICA IDENTITY DEFAULT`. Each function switches a table that is already in the publication to its own form.

**Replica identity.** Postgres refuses `UPDATE` and `DELETE` on a published table that has no usable replica identity. So both functions refuse a table, or a partition, that has neither `REPLICA IDENTITY DEFAULT` with a primary key nor `FULL`. A migration that runs later and drops the primary key, sets `REPLICA IDENTITY NOTHING`, or (under a key-only list) changes the key, makes the app's own writes to that table fail until it is fixed. The check at `Start` catches this only when it already happened before the restart. After changing a key under a key-only list, run `InstallPublicationKeysOnly` again.

## Large transactions on the logical feed

Postgres decodes the write-ahead log in commit order, so a transaction's changes reach the server only at or after its commit, and every later commit waits behind it. Up to `logical_decoding_work_mem` (64 MB by default), Postgres keeps a transaction's decoded changes in memory. Beyond that, the logical feed has them **streamed**: sent while the transaction still runs, and handed to the engine at its commit, or dropped if it aborts.

- **Table-dirty only.** A streamed transaction is reported as "these tables changed", without row keys, and its fences, events and re-checks follow at its commit, as usual. Subscriptions on those tables re-run in full once, which is what a transaction this large would cost them anyway.
- **Savepoints.** If a streamed transaction rolls back a savepoint, the events it emitted before that rollback are not delivered, even those outside the savepoint: Postgres streams them without saying which savepoint they belong to, and events are at-most-once. Re-checks and the call's result are unaffected.
- **What streaming saves.** A long batch job is decoded while it runs, so calls that commit after it are not held while all of it is read back and decoded at its commit.
- **What it does not save.** The changes still have to be read: a short burst that writes faster than the walsender decodes still delays the calls behind it, by the time it takes to read through it.
- **Spilling to disk.** Without streaming (protocol 1), a transaction larger than `logical_decoding_work_mem` spills to disk on the database server, once per node, and is read back at its commit, while every later commit on that node waits. Streaming needs PostgreSQL 14 or later, which the logical feed requires anyway.
- **Monitoring.** `pg_stat_replication_slots` shows a slot's `stream_txns` and `spill_txns`. With streaming, spills should be rare: Postgres still spills a transaction it cannot stream at that moment, for example in the middle of a TOAST write.

## Moving to the logical feed

A running app moves from `FeedNotify` to `FeedLogical` one node at a time, without downtime. During the move some nodes run each feed, and both kinds of nodes serve the same subscriptions correctly. Only events are affected (below).

1. **Prepare the server.**
   - Set `wal_level = logical`. This needs a Postgres restart.
   - Make room in `max_replication_slots` and `max_wal_senders`: one slot and one sender per node, plus whatever standbys and other replication tools use.
   - Set `max_slot_wal_keep_size`.
   - Give the app's role `REPLICATION`, or your provider's equivalent.
2. **Publish the tables.** Run `InstallPublication` for every table in a publication's `Tables`, next to `InstallTriggers`, which stays.
3. **Restart every node with `FeedMigrating: true`**, still on `FeedNotify`. From now on, every re-check is sent both ways.
4. **Roll the nodes to the logical feed** one at a time:
   - set `Feed: FeedLogical` and `ReplicationDSN`;
   - set a stable `SlotName`, so a restarted node resumes its slot;
   - keep `FeedMigrating: true`.

   A logical node keeps a `LISTEN` on re-checks for as long as `FeedMigrating` is set, so it also gets re-checks from notify nodes that have not reached step 3.
5. **Once every node runs the logical feed,** restart them with `FeedMigrating` off.
6. **Run `UninstallTriggers`** for the tables. Writes stop paying for the triggers only from here.

**During the move:**

- **Re-checks** reach every node, whatever its feed: a lost one would leave a permission granted.
- **Events** are not sent both ways. An event emitted on a node of one feed reaches only subscribers on nodes of the same feed. Events are at-most-once, and nothing could de-duplicate a second copy.
- **Fences and method results** are unaffected: each node waits for its own calls on its own feed.

**Rolling back** is the same steps in reverse, as long as the triggers are still installed. After `UninstallTriggers`, a notify-mode node refuses to start, because `Start` checks the triggers. Run `InstallTriggers` first.

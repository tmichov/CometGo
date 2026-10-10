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

app.Start(ctx)
http.Handle("/_comet", app)
```

Register every publication and method before `Start` and before serving requests. The registries are not safe to change while connections are being served.

## App

| Identifier | Description |
|---|---|
| `New(cfg Config) *App` | Creates an app and applies defaults to `cfg`. Panics if `cfg.DB` is nil. |
| `(*App) Start(ctx)` | Starts the Postgres listener and the tick engine in background goroutines; they run until `ctx` is done. Panics if called twice. |
| `(*App) ServeHTTP(w, r)` | `App` is an `http.Handler`. It upgrades the request to a CometGo WebSocket. Mount it at `/_comet`, the client's default URL. |
| `Subprotocol` | The WebSocket subprotocol, `"cometgo.1.json"`. A request that does not offer it gets HTTP 426 with `{"supported":["cometgo.1.json"]}`. |

## Config

Only `DB` is required. Zero or negative durations and limits are replaced with their defaults.

| Field | Default | Meaning |
|---|---|---|
| `DB *pgxpool.Pool` | required | Pool used for publication queries, method transactions and the `LISTEN` connection. |
| `Authenticate` | nil | Validates bearer tokens. See [Authentication](#authentication). If nil, every token is rejected with `UNAUTHENTICATED`. |
| `AuthenticateRequest` | nil | Resolves a cookie-session identity at WebSocket upgrade. See [Authentication](#authentication). |
| `Build string` | `""` | Identifies the current client build. Sent to clients in `welcome`, where the client exposes it as `comet.serverBuild`. The server does not compare it with anything. |
| `OriginPatterns []string` | none | Extra allowed `Origin` hosts for the WebSocket upgrade, as host patterns (e.g. `"app.example.com"`, `"*.example.com"`). Same-origin requests are always allowed; other origins are refused. |
| `Heartbeat` | 25s | The server pings every interval and closes the connection with 4408 if a ping gets no pong within twice the interval. Also sent to the client, which uses it for its own liveness check. |
| `MinTickInterval` | 20ms | Minimum time between engine ticks. Bursts of changes inside the interval are coalesced into one re-run. |
| `FenceTimeout` | 5s | How long a method result waits for its writes to reach the caller's subscriptions. After that the result is sent with `lag: true`. |
| `QueryTimeout` | 30s | Hard `statement_timeout` for publication queries. A query that exceeds it ends its subscriptions with `TIMEOUT`. |
| `MaxSubs int` | 500 | Subscriptions per connection. A subscription over the limit is ended with `RATE_LIMITED`. |
| `MaxInflight int` | 32 | Concurrent method calls per connection. A call over the limit gets `RATE_LIMITED` with `retryAfterMs: 100`. |
| `MaxMsg int` | 1 MiB | Largest message the server accepts from a client. A larger message closes the connection. |
| `MaxRows int` | 10,000 | Row cap per publication result. A larger result ends its subscriptions with `RESULT_TOO_LARGE`. |
| `ResumeGrace` | 60s | How long a session outlives a lost connection, waiting for the client to resume it ([protocol §7.2](protocol.md#72-session-resume)). Unlike the other durations, a **negative** value disables resume, and clients then resync on every reconnect. See [Session resume](#session-resume). |
| `ResumeBuffer int` | 1 MiB | Bytes of not-yet-acknowledged messages kept per session for replay, at most 4 MiB (half the outbound queue limit, since a resume sends the whole replay at once). A disconnected session that outgrows it ends, and its client resyncs. |
| `Logger *slog.Logger` | `slog.Default()` | Where the framework logs, including the details of `INTERNAL` errors. |

`MaxSubs`, `MaxInflight` and `MaxMsg` are also advertised to the client in `welcome.limits`.

## Publications

```go
func Publish[A any](a *App, name string, opts PubOptions, fn func(c *SubCtx, args A) (Query, error))
```

`Publish` registers a live query under `name`. When a client subscribes, the subscription's args are JSON-decoded into `A` (unknown fields are rejected) and `fn` returns the SQL to run. The query's result is sent as the subscription's initial snapshot, and it is re-run and diffed every time one of `opts.Tables` changes.

`Publish` panics if `name` is already registered or `opts.Tables` is empty.

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

Each distinct `(publication, args, scope)` combination has one server-side **observer**, shared by every subscription that matches it. `fn` is called **once**, when the observer is created, and the `Query` it returns is reused for every re-run until the last subscription to it goes away. So:

- Everything the query depends on must be in `Query.SQL` and `Query.Args`, and the scope must separate callers whose queries would differ (see [Scope](#scope)).
- `fn` runs on the engine's loop, so keep it fast. Doing an access check against the database there delays every subscription on the server.
- Returning an error from `fn` ends the subscription with that error. Return a `*cometgo.Error` (e.g. `ErrForbidden`) to choose what the client sees; any other error becomes `INTERNAL`.
- Bad args (wrong types, unknown fields) end the subscription with `BAD_REQUEST`. An unknown publication name ends it with `NOT_FOUND`.

All observers that need re-running in one tick run inside one `REPEATABLE READ READ ONLY` transaction, so every update a client receives comes from one consistent database snapshot.

### PubOptions

| Field | Default | Meaning |
|---|---|---|
| `Tables []string` | required | Every table the query reads, as `"table"` (meaning `public.table`) or `"schema.table"`. A change to any of them re-runs the query. Each must have triggers from `InstallTriggers`. Write names exactly as Postgres stores them (normally lowercase): they are compared as strings, so `"Todos"` never matches changes to `todos`. |
| `Key []string` | `["id"]` | The result columns that uniquely identify a row. They must be present in the `SELECT` list and must not be null. A missing key column fails the query with `INTERNAL`. |
| `Unordered bool` | false | Declares that row order carries no meaning. The server then sends no positions, and the client's row order is arbitrary. Leave it false and use `ORDER BY` when order matters. |
| `Scope Scope` | `ScopeUser` | Which connections may share one observer. See below. |
| `Events bool` | false | Allows [events](#events) to be sent to this publication's subscriptions. |

### Scope

```go
const (
	ScopeUser   Scope = iota // default
	ScopeShared
)
```

- **`ScopeUser`** (the default) shares an observer only between connections whose `Identity.UserID` is the same. Anonymous connections share with each other. This is the safe choice for any query that depends on who is asking, **as long as `UserID` alone determines the query**. The scope key is `UserID` and nothing else: an identity with an empty `UserID` shares with anonymous connections, and two identities with the same `UserID` but different `Data` (say, a different tenant or role) share one observer. If the query depends on anything in `Identity` besides `UserID`, make `UserID` unique per distinct query (for example `tenant + ":" + user`) or pass that value in the subscription args and check it in `fn`.
- **`ScopeShared`** shares one observer between every connection. Only use it when the query does not depend on the caller's identity at all, such as a public feed. Because `fn` runs once per observer, a `ScopeShared` publication that reads `c.Identity` serves the **first** subscriber's result to everyone else.

### Query and SubCtx

```go
type Query struct {
	SQL  string
	Args []any
}

type SubCtx struct {
	Context  context.Context
	Identity *Identity // nil when anonymous
}
```

The query is re-run in full on every change, so it must be deterministic for a given snapshot: add `ORDER BY` to any publication that is not `Unordered`. `Args` are passed to pgx as query parameters.

`SubCtx.Context` is the engine's context, not the client's.

## Methods

```go
func Method[A, R any](a *App, name string, fn func(c *Call, args A) (R, error))
```

`Method` registers a server function under `name`. The call's args are JSON-decoded into `A` (unknown fields are rejected with `BAD_REQUEST`), and the returned `R` is JSON-encoded as the call's result. `Method` panics if `name` is already registered.

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

Writes through another connection (the pool passed as `Config.DB`, or any other connection) still update subscriptions, because the triggers see them, but they carry no fence: the call can resolve before its effects reach the client.

Other details:

- Calls on one connection run concurrently, up to `Config.MaxInflight`. A client that needs two calls in order should await the first.
- `c.Context` is **not** cancelled when the client disconnects. A call that has started runs to completion and commits or rolls back on its own. If the client resumes its session, it gets the result; otherwise it reports `CONNECTION_LOST`, because it cannot know which happened.
- A client can send a call with `noFence`, which commits without the fence and returns as soon as the transaction commits. Use it for calls that do not write.
- An unknown method name returns `NOT_FOUND`.

## Events

Events are ephemeral messages to a publication's subscribers, for things that do not belong in a table: progress, typing indicators, "job finished". The publication must set `PubOptions.Events`.

```go
func (a *App) Emit(ctx context.Context, pubName string, args any, scope, topic string, data any) error
func (c *Call) EmitAfterCommit(pubName string, args any, scope, topic string, data any) error
```

An event is addressed to one observer: the publication, its args and its scope. It is delivered to every subscription bound to that observer, and the client receives it through `sub.onEvent(topic, fn)`.

- `args` must encode to the same JSON value the client sent when subscribing (key order and whitespace do not matter). The comparison is on JSON, not on the decoded args: a Go struct without `json` tags encodes `ProjectID`, which does not match a client's `projectId`, and fields the client omitted must be omitted too. Tag your args types and use the same type for the publication and `Emit`. `nil` matches a subscription made without args. An event that matches no subscription is dropped without an error.
- `scope` is the user ID for a `ScopeUser` publication, and must not be empty: anonymous subscribers cannot be targeted. It is ignored for `ScopeShared`.
- `data` is JSON-encoded.

`Emit` sends the event at once. `EmitAfterCommit` sends it from inside a method's transaction: the event is sent only if the method commits, and the client receives it after the update carrying the method's writes and before the method's result. Use it whenever the event refers to data the method writes.

```go
cometgo.Method(app, "jobs.finish", func(c *cometgo.Call, a FinishArgs) (struct{}, error) {
	if _, err := c.Tx.Exec(c.Context, "UPDATE jobs SET done = true WHERE id = $1", a.ID); err != nil {
		return struct{}{}, err
	}
	return struct{}{}, c.EmitAfterCommit("jobs.mine", nil, c.Identity.UserID, "finished", map[string]any{"id": a.ID})
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
| `CodeCanceled` | `CANCELED` | never in v1-core (call cancellation is planned) |
| `CodeResultTooLarge` | `RESULT_TOO_LARGE` | a publication result exceeds `MaxRows` |
| `CodeUnavailable` | `UNAVAILABLE` | never |
| `CodeInternal` | `INTERNAL` | any error that is not a `*cometgo.Error` |

Application-specific codes should use the `app.` prefix, e.g. `NewError("app.quota_exceeded", "…")`.

## Authentication

A connection is either anonymous or bound to an `*Identity`:

```go
type Identity struct {
	UserID    string    // used by ScopeUser to separate users
	User      any       // sent to the client; defaults to {"id": UserID}
	ExpiresAt time.Time // optional; see below
	Data      any       // free-form, server-side only
}
```

Publications and methods see it as `c.Identity` (nil when anonymous). `User` is what the client sees as `comet.user`; `Data` never leaves the server.

There are two ways to authenticate.

### Bearer tokens: `Authenticate`

```go
Authenticate func(ctx context.Context, token string) (*Identity, error)
```

Called with the token the client sends when it connects and every time it calls `setToken`.

- Return an identity to accept the token.
- Return a `*cometgo.Error` (typically `UNAUTHENTICATED`) to reject it with that error, or `(nil, nil)` to reject it as `UNAUTHENTICATED`.
- An invalid token never closes the connection: the connection continues as anonymous and the client is told the token was rejected.

When the identity changes (login, logout, a different user), every subscription on the connection is re-bound to the new identity and reset in one atomic update. Subscriptions the new identity may not read end with the error their publication returns. Calls already running keep the identity they started with. A refresh that keeps the same `UserID` changes nothing on the connection's subscriptions.

### Cookie sessions: `AuthenticateRequest`

```go
AuthenticateRequest func(r *http.Request) (*Identity, error)
```

Called with the upgrade request, so it can read session cookies. Its result is used only when the client connects without a token.

- Return `(nil, nil)` for an anonymous connection.
- Return an error to refuse the session: if the client connected without a token, the connection is closed with code 4401.

### Expiry: `Identity.ExpiresAt`

If `ExpiresAt` is set, the server tells the client when the identity expires (`welcome.authExp` / `authRes.authExp`, in Unix seconds). When that time passes without a new token, the connection is downgraded to anonymous, every subscription is reset as for a logout, and the server sends an `AUTH_EXPIRED` auth result. The TypeScript client does not surface that code: the app sees `comet.user` become `null` and `onChange` fire, the same as a logout. The client should call `setToken` with a fresh token before then. Proactive refresh by the client library and the HTTP refresh endpoint are *planned* ([protocol §3.2](protocol.md#32-mid-session-auth)).

## Session resume

A client that loses its connection briefly (a network blip, a laptop waking up) resumes its session instead of starting over. The server keeps the session for `Config.ResumeGrace` after the socket drops: subscriptions stay bound, running calls finish, and everything that would have been sent is kept in a replay buffer of up to `Config.ResumeBuffer` bytes. When the client reconnects in time, it gets exactly the messages it missed: no snapshots are re-sent, and no call result is lost.

The session is not resumed, and the client falls back to a full resync, when:

- the server restarted,
- the grace window passed,
- the buffer overflowed while the client was away,
- the reconnecting client authenticates as a different user than the session's.

A token refresh for the same user is fine. A protocol error, or a deliberate close by the client (code 1000/1001, e.g. a page unload), ends the session for good; a heartbeat timeout or a slow-consumer close leaves it resumable. An anonymous session is resumable by whoever holds its session id, which the client keeps in memory only.

**Multiple nodes.** Sessions live in the memory of the node that served them. Behind a load balancer, route a client's reconnects to the same node (sticky sessions, e.g. by cookie or client IP). Without sticky routing everything still works, but a reconnect that lands on another node resyncs.

Memory cost: each disconnected session holds its subscriptions plus at most `ResumeBuffer` bytes for up to `ResumeGrace`. Connected sessions hold the unacknowledged tail, usually about one second of output. Set `ResumeGrace` negative to turn resume off.

## InstallTriggers

```go
func InstallTriggers(ctx context.Context, db *pgxpool.Pool, tables ...string) error
```

Installs the change-feed triggers that tell CometGo which rows changed. Run it at startup or from a migration, for every table named in any publication's `Tables`. It is idempotent and installs all the given tables in one transaction.

Requirements:

- Each table must already exist and must have a primary key.
- Tables are named `"table"` (meaning `public.table`) or `"schema.table"`.
- Postgres 11 or newer (the triggers use `EXECUTE FUNCTION`; the protocol spec's "PG 10+" is the minimum for transition tables, not for this code).

Per table it creates statement-level `AFTER INSERT`, `AFTER UPDATE`, `AFTER DELETE` and `AFTER TRUNCATE` triggers named `cometgo_ins`, `cometgo_upd`, `cometgo_del` and `cometgo_trunc`, and their functions in the table's schema. Each statement sends one notification on the channel `comet` listing the changed primary keys, or a "whole table changed" marker when more than 1,000 rows changed or the keys do not fit in one notification. In v1-core any notification for a table re-runs every observer that reads it.

If the server's `LISTEN` connection drops, notifications sent meanwhile are lost, so on reconnect the server re-runs every observer. See [protocol §11](protocol.md#11-postgres-change-feed-server-internal-v1) for the payload format.

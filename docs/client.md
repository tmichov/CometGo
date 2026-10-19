# Client

The TypeScript client lives in [`client/`](../client) as the package `@cometgo/client`. It has no runtime dependencies. It is built to ES modules with type declarations in `client/dist`, and has two entry points: `@cometgo/client` (this page) and `@cometgo/client/react` (the [React bindings](react.md)). Only the second one imports React, which is an optional peer dependency.

Install it from npm:

```sh
npm install @cometgo/client
```

Inside this repository, `client` is an npm workspace, so `npm install` at the root links it into `node_modules/@cometgo/client` and the todo example imports it by name. The link points at the built `dist/`, so run `npm run build:client` (or `npm run build` / `npm run typecheck`, which do it first) after changing `client/src`.

```ts
import { connect, CometError } from "@cometgo/client";

const comet = connect();
const todos = comet.subscribe<Todo>("todos.all");
todos.subscribe(() => render(todos.rows));

await comet.call("todos.add", { title: "Ship v1" }); // the new row is already in todos.rows
```

## connect

```ts
function connect(opts?: ConnectOptions): Comet
function connect<S extends Schema>(opts?: ConnectOptions): TypedComet<S>
```

Creates a `Comet` and starts connecting immediately. With a schema generated from the server, `connect<Schema>()` returns the same object typed so that method and publication names, args, results and rows are checked at compile time. See [Typed clients](codegen.md). One `Comet` holds one WebSocket and reconnects on its own until you call `close()`.

| `ConnectOptions` field | Default | Meaning |
|---|---|---|
| `url?: string` | `ws(s)://<page host>/_comet` | WebSocket URL. A relative URL resolves against the page, and `http(s)` becomes `ws(s)`. Required outside a browser. An invalid URL makes `connect` throw. |
| `token?: TokenSource` | none | Bearer token. Omit it to use cookie sessions. |
| `build?: string` | none | This client's build id, sent to the server in `hello`. |
| `WebSocket?: typeof WebSocket` | global `WebSocket` | WebSocket implementation, for environments without a global one. |
| `routeKey?: string` | a random id per `Comet` | Sent as the `route` query parameter on every connect, for a load balancer that hashes on it ([routing to nodes](server.md#routing-to-nodes)). Set it to the tenant or user id so their connections share a node. It shows up in access logs, so use an opaque id, never an email address. Any `route` already in `url` is replaced. The key is fixed for the `Comet`'s lifetime: `setToken` to another tenant keeps the old key, so create a new connection to change it. |

```ts
type TokenSource = string | null | (() => string | null | Promise<string | null>);
```

A function token source is called on every (re)connect, so it can return a freshly refreshed token. It is also called before the identity expires, to refresh it (see [Auth](#auth)). If it throws, the client logs a warning and does not connect: it tries again (calling the function again) with the usual reconnect backoff, and `status` stays `"disconnected"` meanwhile. Connecting anonymously instead would end a resumable session and look like a logout. So throw only for failures worth retrying (a network error reaching your auth service), and return `null` when there is no token and the client should connect anonymously.

`SUBPROTOCOL` is the WebSocket subprotocol the client offers, `"cometgo.1.json"`.

## Comet

| Member | Description |
|---|---|
| `status: ConnectionStatus` | `"connecting"`, `"connected"` (after the server's `welcome`) or `"disconnected"`. |
| `user: unknown` | The authenticated user as the server sent it (`Identity.User`), or `null` when anonymous. |
| `serverBuild?: string` | The server's `Config.Build` from the last `welcome`. Compare it with your own build to offer a reload. |
| `onChange(fn): () => void` | Calls `fn` whenever `status` or `user` changes. Returns an unsubscribe function. |
| `subscribe<T>(name, args?): Subscription<T>` | Subscribes to a publication. See [Subscriptions](#subscriptions). |
| `call<R>(m, args?, opts?): Promise<R>` | Calls a method. See [Calls](#calls). |
| `callWithStatus<R>(m, args?, opts?): Promise<{ value: R; lag: boolean }>` | Like `call`, but also reports `lag`. |
| `setToken(token: TokenSource): Promise<unknown>` | Logs in, refreshes, or logs out. See [Auth](#auth). |
| `close(): void` | Closes the connection for good and stops reconnecting. It also removes the `online` and `visibilitychange` listeners the client added, so a closed client can be garbage-collected. Calls still waiting are rejected: those already sent with `CONNECTION_LOST` (their outcome is unknown), those never sent with `CANCELED`. A call made after `close()` is rejected at once with `CANCELED`. Subscriptions still `loading` end with `CANCELED` (their `ready` rejects); those with data keep their rows. A subscription made after `close()` is returned already ended with `CANCELED`, and nothing is sent. |

## Subscriptions

```ts
const sub = comet.subscribe<Task>("tasks.byProject", { projectId: "9" });
```

`args` is sent to the server as JSON and decoded into the publication's args type. `T` describes the decoded row: each row is a plain object with one property per result column, decoded per [Type decoding](#type-decoding).

| `Subscription<T>` member | Description |
|---|---|
| `rows: readonly T[]` | The current rows, in server order. The array is replaced, never mutated, on every change, and a row whose values did not change keeps the same object. |
| `status: SubStatus` | See below. |
| `error?: CometError` | Set when the server ended the subscription with an error. |
| `ready: Promise<void>` | Resolves when the first snapshot has arrived. Rejects with the `CometError` if the subscription is ended with an error before that. |
| `subscribe(fn): () => void` | Calls `fn` after every change to `rows` or `status`. Returns an unsubscribe function. |
| `getSnapshot(): readonly T[]` | Returns `rows`. |
| `stop(): void` | Unsubscribes and sets `status` to `"ended"`. It does not notify listeners. |
| `onEvent<D>(topic, fn: (data: D) => void): () => void` | Calls `fn` for each [event](#events) the server sends to this subscription with this topic. Returns an unsubscribe function. |
| `name`, `args` | What was subscribed to. |
| `id: number` | The subscription's wire id. It can change if the client has to re-request a fresh snapshot. |

`subscribe` and `getSnapshot` are bound arrow functions, so they work directly with React:

```ts
const rows = useSyncExternalStore(sub.subscribe, sub.getSnapshot);
```

```ts
type SubStatus = "loading" | "ready" | "resyncing" | "ended";
```

- **`loading`**: waiting for the first snapshot; `rows` is empty.
- **`ready`**: `rows` is the server's current result.
- **`resyncing`**: the connection dropped or is being re-established. `rows` keeps the last known data, which may be stale; dim it if you like.
- **`ended`**: the subscription is over. Either you called `stop()`, which leaves `rows` as it was, or the server ended it, which empties `rows` and sets `error` when it was a failure (e.g. `FORBIDDEN`, `NOT_FOUND`, `RESULT_TOO_LARGE`, `TIMEOUT`, `OVERLOADED`).

Every update from the server is applied as one transaction: all subscriptions it touches change together, and each notifies its listeners once.

### Events

A publication with events enabled on the server can send ephemeral messages to its subscribers, such as progress updates:

```ts
const jobs = comet.subscribe("jobs.mine");
const off = jobs.onEvent<{ id: string }>("finished", ({ id }) => toast(`Job ${id} finished`));
```

Events are at-most-once. Events sent while the client is disconnected are lost, even when the session is resumed, and events for a subscription that has ended or been stopped are dropped. An event the server emits from a method (`EmitAfterCommit`) arrives after the method's writes are in `rows` and before the call's promise resolves. A listener that throws is logged and does not affect the others.

## Calls

```ts
const id = await comet.call<string>("todos.add", { title: "Ship v1" });
const { value, lag } = await comet.callWithStatus("todos.add", { title: "Ship v1" });
```

- **Read-your-writes.** The promise resolves only after the method's writes are reflected in this client's subscriptions, so the UI is already up to date when `await` returns.
- **`lag`.** If the writes did not arrive within the server's `FenceTimeout` (5 s by default), the call still resolves, with `lag: true` from `callWithStatus`. `call` resolves the same way but does not report it.
- **`{ noFence: true }`** skips that wait. A call whose method writes nothing already returns without it, so use `noFence` for calls that write but don't need to wait for their update.
- **`{ signal }`** takes an `AbortSignal` to cancel the call. If the call has not been sent yet, it is rejected at once with `CANCELED` and never sent. Otherwise the client asks the server to cancel, and the promise settles from the server's answer: `CANCELED` if the method had not committed (nothing was written), or its real result if the cancel came too late. Aborting does not settle the promise by itself, because only the server knows whether the method committed.

  ```ts
  const ac = new AbortController();
  const p = comet.call("reports.build", { id }, { signal: ac.signal });
  cancelButton.onclick = () => ac.abort();
  ```
- **`{ idempotent: true }`** attaches a generated idempotency key, so the call survives a disconnect even when the session cannot be resumed: instead of failing with `CONNECTION_LOST`, it is re-sent on the new session, and the server runs the method at most once (see [Server: Idempotency keys](server.md#idempotency-keys)). Use it for writes that must not happen twice. **`{ ik: "..." }`** passes a key of your own instead, e.g. a random UUID stored with a form so a retry after a page reload is also deduplicated. It must be unguessable (generate it with `crypto.randomUUID()` or similar, never from an id), because anonymous callers share one key scope.
- **Retries of idempotent calls** on a new session are held until its `welcome`, and re-sent only if the server supports idempotency keys, the user is the same one the call ran as, and the call was first sent less than an hour ago. Otherwise the call is rejected with `CONNECTION_LOST`. A call cut off before its connection's `welcome` arrived is rejected too, unless the session resumes, since the user it ran as is unknown. The hour stays inside the server's default 24h `IdempotencyTTL`; do not set that below an hour.
- **Errors.** A failed call rejects with a `CometError` carrying the server's code (see below).
- **Before the connection is open,** calls are queued and sent when it opens.

## Auth

**Tokens.** Pass `token` to `connect`, or call `setToken` at any time:

```ts
await comet.setToken(accessToken); // log in, or refresh; resolves with comet.user
await comet.setToken(null);        // log out
```

`setToken` resolves with the new `comet.user`, or rejects with a `CometError` such as `UNAUTHENTICATED`. A rejected token changes nothing on the connection: the current user stays logged in, with the same subscriptions, until their identity expires or you call `setToken(null)`. The rejected token is not kept either: the client goes back to the token it had before, and a reconnect sends that one. A `setToken` made while an earlier one is still waiting is never undone by the earlier one's rejection. If the connection drops before the server answers, `setToken` rejects with `CONNECTION_LOST` and the new token stays stored, because the server may have accepted it. If the client is not connected, `setToken` only stores the token for the next connection and resolves with the current `user`. That includes a connection that is still opening: its `hello` carries the new token, and `comet.user` changes when the server's welcome arrives (`onChange` fires).

When the user changes, the server resets every subscription to the new user's data in one update; subscriptions the new user may not read end with an error.

**Cookie sessions.** Omit `token`. The server reads the session cookie when the WebSocket opens (`AuthenticateRequest`). If the server refuses the session it closes the connection with 4401. The client treats that like any other drop and keeps reconnecting with backoff, and each attempt is refused again: queued calls stay pending and subscriptions stay `loading` or `resyncing`. There is no dedicated signal for it in v1-core, so if a cookie session can expire, check it over HTTP (or watch `comet.status` stay `"disconnected"`) and send the user to log in.

**Expiry.** If the server's identity has an expiry and no new token arrives in time, the connection becomes anonymous: `comet.user` becomes `null`, `onChange` fires, and subscriptions are reset as for a logout. When `token` (from `connect` or the last `setToken`) is a function and the server reports the identity's expiry (`authExp`), the client calls it again at about 80% of the time left and sends the result as `setToken` would. A refused refresh keeps the current user and is retried at 80% of the time then left (at least 5 s apart). With a string token the client cannot get a new one, so call `setToken` yourself before it expires. The refresh stops on `close()`.

## CometError

```ts
class CometError extends Error {
  readonly code: string;
  readonly details?: unknown;
  readonly retryAfterMs?: number;
}
```

`message` is the server's `msg` (or the code). The codes are the server's [error codes](server.md#codes), plus one the client creates itself:

| Code | Meaning |
|---|---|
| `CONNECTION_LOST` | The connection dropped after the call (or `setToken`) was sent and before its result arrived, and the session could not be resumed (see [Reconnects](#reconnects)). The outcome is **unknown**: the method may or may not have committed. The client never re-sends it on a new session. Retry only if the method is safe to run twice. A call made with `idempotent` or `ik` gets this only when it cannot be retried safely (see [Calls](#calls)) or after `close()`. |
| `VALIDATION` | `details.fields` maps field names to messages. |
| `RATE_LIMITED` | Too many calls in flight; `retryAfterMs` says when to retry. |
| `INTERNAL` | A server bug. `details.ref` matches a line in the server log. |

```ts
try {
  await comet.call("todos.add", { title });
} catch (e) {
  if (e instanceof CometError && e.code === "VALIDATION") showFieldErrors((e.details as any).fields);
  else throw e;
}
```

## Reconnects

The client reconnects by itself:

- **Backoff.** Exponential with full jitter, starting at 500 ms and capped at 30 s, reset after a successful connection. In a browser it also retries immediately on the `online` event and when the tab becomes visible.
- **Server shutdown.** When the server announces a graceful shutdown (`goaway`, [protocol §7.3](protocol.md#73-server-shutdown)), the client waits the delay the server chose (0–5 s) before reconnecting, instead of the backoff. Calls made in the meantime are held and sent on the new connection. Calls already sent resolve normally if their result arrives before the server closes the connection.
- **Liveness.** If nothing arrives for the server's heartbeat interval (25 s by default), the client sends a ping. After the heartbeat interval plus 10 s of silence, it closes the connection (code 4408) and reconnects.
- **Session resume.** When the server supports it, the client first tries to resume its session ([protocol §7.2](protocol.md#72-session-resume)). The server replays whatever the client missed, so subscriptions get no snapshot and calls in flight still get their results. The client acks what it has received about once a second, so the server can drop it from the replay buffer.
- **Subscriptions** survive. While disconnected, `status` is `"resyncing"`. On a resumed session it goes back to `"ready"` and the missed updates are applied as usual. Otherwise each subscription is re-sent; the server sends a fresh snapshot, and the client swaps it in only once it is complete. Unchanged rows keep their objects, so the UI does not flicker.
- **Calls** that were queued but never sent go out on the new connection. Calls (and `setToken`) that were sent and had no result yet depend on the reconnect:
  - **Resumed:** they are re-sent with their original ids. The server runs each call at most once and delivers its result.
  - **Not resumed** (the server restarted, the reconnect reached another node, the grace window (60 s by default) passed, or the user changed meanwhile): they are rejected with `CONNECTION_LOST`, except calls made with `idempotent` or `ik`, which are re-sent with the same key under the conditions in [Calls](#calls).
  - **Server without resume:** the same as not resumed, decided once the new connection's `welcome` arrives for calls with a key, and as soon as the connection drops for the rest.

  So with resume, a call sent just before a drop stays pending until the client reconnects, however long that takes. `close()` rejects it at once.

## Type decoding

Each subscription receives its column types once, and the client decodes every value by its column's type:

| Postgres type | Wire code | TypeScript value |
|---|---|---|
| `int2`, `int4` | `i4` | `number` |
| `int8`, `bigserial` | `i8` | `string` (decimal). JavaScript numbers cannot hold every int8. |
| `float4`, `float8` | `f8` | `number` (including `NaN`, `Infinity`, `-Infinity`) |
| `numeric` | `num` | `string` (exact decimal) |
| `bool` | `bool` | `boolean` |
| `text`, `varchar`, `char`, `name` | `text` | `string` |
| enum | `enum` | `string`. Arrays of an enum are `[]enum`, decoded to `string[]`. |
| `uuid` | `uuid` | `string` (lowercase) |
| `timestamptz` | `tstz` | `Date`. `Date` keeps milliseconds only; the wire value has microseconds. `infinity`/`-infinity` stay strings. |
| `timestamp` | `ts` | `string`, `"YYYY-MM-DDTHH:MM:SS.ffffff"` with no time zone |
| `date` | `date` | `string`, `"YYYY-MM-DD"` |
| `json`, `jsonb` | `json` | the parsed JSON value. Numbers beyond 2^53 lose precision. |
| `bytea` | `bytes` | `Uint8Array` |
| one-dimensional array | `[]<code>` | array of the element's decoded values; `null` elements allowed |
| any other type pgx knows | `raw` | `string`, Postgres text form |
| `NULL` | any | `null` |

Opt-in decoders (`bigint` for int8, a Decimal type for numeric, `Temporal.Instant`) are *planned*.

Method arguments and results are plain JSON in both directions; nothing is decoded for you. Send int8 values as strings, matching how they arrive.

## Lower-level exports

| Export | Description |
|---|---|
| `decoderFor(code: string)` | Returns the decoder function for one wire type code, as used above. |
| `RowSet<T>` | The ordered row store a subscription uses internally: applies wire ops, adopts a reset without losing object identity, and materializes rows. Useful for testing. |
| `type Op` | One wire row operation: insert, update, delete or move. See [protocol §4.2](protocol.md#42-tx-the-only-data-bearing-message). |
| `type SubInit` | A subscription's `init` descriptor: columns with type codes, key column indexes, `ordered`. |
| `type ConnectionStatus`, `type SubStatus` | The status unions above. |
| `Comet`, `Subscription<T>` | The classes behind `connect` and `subscribe`. Create them through those functions. |

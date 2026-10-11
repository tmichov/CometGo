# CometGo Wire Protocol — v1

Status: draft. Sections and fields marked **[later]** are reserved and specified, but not part of the first implementation (v1-core). Every [later] feature is additive and negotiated through `caps`; none changes the shape of a core message.

## 0. Model

The server is authoritative. Clients never simulate writes (no optimistic UI in v1).

Everything rests on one server mechanism, the **tick**:

1. Drain pending Postgres change notifications into a dirty set, recording the cutoff `p` (the listener sequence number of the last drained notification).
2. Open one `REPEATABLE READ READ ONLY` snapshot.
3. Re-run every dirty observer (plus any pending initial snapshots) inside that snapshot and diff against its previous result.
4. For each connection, assemble the diffs of its subscriptions into one atomic `tx` message and enqueue it.
5. Release the method fences this tick satisfies (§6.2).

Draining **before** taking the snapshot is required. A notification is only delivered after its transaction commits, so every drained notification is visible to the later snapshot. The reverse order loses updates. Commits that land between the drain and the snapshot just cause a harmless redundant re-run on the next tick.

### 0.1 Observers

An **observer** is the server-side live query behind a subscription. It is keyed by `(publication, canonicalArgs, scopeKey)`. The `scopeKey` is whatever the publication declares it depends on:

- `user.id` by default (safe),
- `tenant.id`,
- or nothing, for a shared/public publication.

Each observer:

- holds its current result (ordered keys and full rows),
- is re-run at most once per tick, however many connections use it,
- has its diff fanned out to every subscription bound to it.

Per-connection server state is only `subId → observer`, plus the outbound queue and, with session resume, a bounded replay buffer (§7.2). There is no per-connection document cache (no DDP-style mergebox).

## 1. Transport

- **Endpoint.** WebSocket at `/_comet`, on the same origin as the UI. The server checks `Origin` (protection against cross-site WebSocket hijacking).
- **Negotiation.**
  - Version and encoding are negotiated through `Sec-WebSocket-Protocol`. The client offers `cometgo.1.json`; **[later]** `cometgo.1.cbor`.
  - If the server supports none of them, it refuses the upgrade with HTTP 426 and lists the supported protocols.
  - The major version lives in the subprotocol name. Within a major version, changes are additive only: receivers ignore unknown fields and unknown `caps`.
- **Framing.**
  - One WebSocket frame carries exactly one message: a JSON object with a string discriminator `t`.
  - Only `tx` messages may be split across frames (§4.3).
  - Max inbound message size: 1 MiB by default (sent to the client in `welcome.limits`).
  - Outbound frame target: about 256 KiB.
- **Compression.** `permessage-deflate` is supported and configurable. **[later]** tuning.

## 2. Sequencing

- Every **stateful** server→client message (`tx`, `res`, `authRes`, `evt`) carries `seq`. This is a per-session counter that increases strictly with each message.
- `seq` gives one total order per connection across data, method results and auth results.
- `ping`, `pong` and `ack` carry no `seq`.
- Client→server messages carry no sequence numbers.
- Session resume uses it: the client acks `seq` and the server replays messages after the last ack (§7.2). Without resume, `seq` is informational.
- All parts of a split `tx` (§4.3) carry the same `seq`. A client counts that `seq` as received only once the final part has arrived.

## 3. Handshake and auth

### 3.1 Handshake

The handshake is **pipelined**: the client may send `sub`/`call` immediately after `hello`, without waiting for `welcome`. The server processes them after `hello`.

```json
→ {"t":"hello","v":1,"caps":[],"auth":{"token":"eyJ..."},"build":"web@2026.10.10-1"}

← {"t":"welcome","session":"s_9fK2…","epoch":"b_7d1e…","resumed":false,
   "hb":25000,"user":{"id":"42"},"authExp":1760101200,
   "limits":{"maxSubs":500,"maxInflight":32,"maxMsg":1048576},
   "build":"web@2026.10.10-1","caps":[]}
```

| Field | Meaning |
|---|---|
| `auth` | Optional. Bearer access token. Never put the token in the URL, because it leaks into access logs. Omit `auth` to use cookie-session mode, where the server reads the session cookie during the upgrade. |
| `epoch` | Random ID generated each time the server process boots. A client seeing a new `epoch` knows a resume was impossible. |
| `build` | Server's current client build. A mismatch lets the client offer or perform a reload. **[later]**: automatic hot reload. |
| `hb` | Heartbeat interval in ms (§8.2). |
| `user` | The authenticated principal, or `null` for anonymous. |

**Failures:**
- An invalid token in `hello` does not close the connection. The session continues as anonymous and the server sends `authRes{ok:false, err:{code:"UNAUTHENTICATED"}}`.
- An invalid session cookie in cookie mode closes the connection with code 4401.

### 3.2 Mid-session auth

```json
→ {"t":"auth","id":3,"token":"eyJ...new"}        // token:null = logout
← {"t":"authRes","seq":1902,"id":3,"ok":true,"user":{"id":"42"},"authExp":1760102100}
```

- **Token refresh, same principal.** If the claims the bound publications depend on are unchanged (typically only `exp` changed), this is a no-op: no subscription re-runs.
- **Identity changed** (login, logout, switching user or tenant, server-side revocation). The server re-binds every subscription to the observer for the new identity and sends **one** `tx` that:
  - resets every affected sub to its new result (`reset:true`, §4.4), and
  - ends subs the new identity may not access (`end:{code:"FORBIDDEN"}`).

  `authRes` is sent **after** that `tx`. The UI moves from user A's world to user B's world in one render. **[later]**: minimal diff rebind instead of resets.
- **In-flight calls** keep running under the identity they were received with.
- **Expiry without refresh** is treated like logout. The server pushes an `authRes` with no `id`: `{"t":"authRes","seq":…,"ok":false,"err":{"code":"AUTH_EXPIRED"}}`. The client library refreshes proactively at about 80% of the token lifetime.
- **Token refresh flow.** The access token is short-lived. A refresh token lives in an httpOnly cookie and is used only by the HTTP endpoint `/_comet/auth/refresh`.

`id` values for `auth` and `call` share one namespace. The client chooses them as unsigned integers, unique within the session.

## 4. Subscriptions

### 4.1 Client messages

```json
→ {"t":"sub","s":7,"name":"tasks.byProject","args":{"projectId":"9"}}
→ {"t":"unsub","s":7}
```

- **`s`** is a client-chosen unsigned integer and is never reused within a session. Late data for an unsubscribed `s` is dropped by the client.
- **`unsub`** gets no reply.
- **A repeated `sub`** with an `s` that is already active and has the same `name`/`args` is a no-op. With different `name`/`args` it is a `BAD_REQUEST`.
- **`"have":true`** on a `sub` means "I hold stale data for this sub". The server answers with a `reset` entry instead of a plain initial snapshot. Clients use it when resyncing after reconnect (§7.1).
- **Sub args** are at most 64 KiB.

### 4.2 `tx`: the only data-bearing message

```json
{"t":"tx","seq":1843,"subs":[
  {"s":7,
   "init":{"cols":[["id","i8"],["title","text"],["done","bool"],["due","tstz"]],
           "key":[0],"ordered":true,"src":"public.tasks"},
   "ops":[["i","1",["1","Buy milk",false,"2026-10-10T08:00:00.123456Z"],0],
          ["i","2",["2","Ship v1",false,null],1]],
   "ready":true},
  {"s":9,"ops":[["u","555",[2,true]],["d","556"],["m","557",0]]},
  {"s":11,"end":{"code":"FORBIDDEN","msg":"no access to project 9"}}
]}
```

**SubEntry fields** (all optional except `s`):

| Field | Meaning |
|---|---|
| `init` | Sent once per sub, with or before the first ops. Contents: <br>• `cols`: an array of `[name, typeCode]` that fixes the positional row layout. <br>• `key`: the column indexes of the primary key. <br>• `ordered`: whether ops carry positions. <br>• `src`: the schema-qualified table, present only when the publication is a single-table projection that includes the PK. Clients ignore `src` in v1. **[later]** it is used for interning. |
| `reset` | `true` means the ops that follow, up to `ready`, are the sub's complete new result (§4.4). |
| `ops` | Ordered list of row operations, applied sequentially. |
| `ready` | The initial or reset snapshot is complete. |
| `end` | The server has terminated the sub. With an error object (§9) it is a failure; without one it is a normal completion. The client removes the sub's rows. |

**Ops** (tuples):

| Op | Shape | Meaning |
|---|---|---|
| insert | `["i", key, row, idx?]` | The row entered the result set. `row` is positional per `init.cols`. |
| update | `["u", key, [colIdx, val, colIdx, val, …], idx?]` | Patch of changed columns only. Each value is replaced whole. `idx` is present if the row's position changed. |
| delete | `["d", key]` | The row left the result set. Being deleted and dropping out of a LIMIT window are the same thing. |
| move | `["m", key, idx]` | Pure reorder. |

- `idx` appears only when `init.ordered`. It is the row's index **after** applying the op, with ops applied in order.
- Ordering is server-authoritative: the client never sorts, because JavaScript cannot reproduce Postgres collation.
- SQL columns cannot disappear, so there is no "cleared field" concept. `null` is just a value.

**Row identity.** `key` is an opaque canonical string produced by the server. The client uses it only as a map key and never parses it.
- Single-column PK: the value's wire string (`"42"`, a UUID).
- Composite PK: compact JSON array text (`"[\"org_1\",42]"`).

**Client rule.** The client applies one complete `tx` (after its last part) as one store transaction and triggers one reactive flush. A `tx` contains diffs from exactly one committed snapshot.

### 4.3 Splitting

- A `tx` larger than the frame target is split into parts carrying the same `seq`. Every part but the last has `"more":true`.
- Parts of one `tx` are contiguous: only `ping`, `pong` and `ack` may be interleaved between them.
- The client buffers the parts and applies the `tx` when the final part arrives.
- Why split at all: browsers deliver only complete frames. A single huge frame on a slow link would stall `res` and `pong` messages behind it and cause false liveness timeouts.
- The total size of a sub is bounded by the per-observer row cap (§8.3).

### 4.4 Reset semantics (flicker-free)

On a SubEntry with `reset:true`:

1. The client keeps showing its current rows and buffers the incoming `i` ops until `ready:true`.
2. It diffs the buffered result against what it holds and applies only the actual changes, all in one store transaction.

Rows that did not change keep their object identity, so the UI does not re-render them. While a reset is in progress, `sub.status` is `"resyncing"`, so the UI can dim stale data if it wants to.

Resets are used for reconnect resync, identity change and slow-consumer recovery (§8.1).

### 4.5 Events (`evt`)

Ephemeral server→client messages for things that do not belong in a table: progress updates, typing indicators, "job finished" pings.

```json
← {"t":"evt","seq":1852,"s":7,"topic":"progress","data":{"pct":40}}
```

- **Negotiation.** The client offers `"evt"` in `hello.caps`; the server advertises `"evt"` in `welcome.caps`. The server sends `evt` only to clients that offered the cap.
- **Scope.** An event is addressed to one observer, `(pub, args, scope)`, and delivered once to every sub bound to it, each with its own `s`. The publication must enable events on the server.
- **At-most-once.** An event reaches the subs bound when the server processes it, on sessions with an open socket that are not coalescing (§8.1). It is not stored: events sent while a client is away are never delivered, whether it resyncs (§7.1) or resumes (§7.2). An `evt` that was already sent when the socket dropped is in the replay buffer like any stateful message, so a resume can deliver it, once. A session whose `evt` cap differs from the resuming `hello`'s cannot be resumed.
- **Ordering.** `evt` is stateful and carries `seq`. An event follows the `tx` reflecting every change committed before it was emitted. An event emitted inside a method's transaction is sent only if it commits, after the `tx` with the method's writes and before the method's `res` (unless the call used `noFence`).
- **Size.** The encoded event is limited to 7,500 bytes; the server API rejects a larger one.
- **Multi-node.** Events travel through `pg_notify` on the channel `comet_evt`, so an emit on any node reaches subscribers on every node.
- The client drops events for subs it does not know or that have ended.

## 5. Methods

```json
→ {"t":"call","id":12,"m":"tasks.create","args":{"title":"x","projectId":"9"}}
← {"t":"res","seq":1850,"id":12,"ok":true,"val":{"id":"557"}}
← {"t":"res","seq":1851,"id":13,"ok":false,
   "err":{"code":"VALIDATION","msg":"invalid","details":{"fields":{"title":"required"}}}}
```

- **Concurrency.** Calls run concurrently, up to `limits.maxInflight`. A caller that needs ordering awaits. Calls beyond the limit get `RATE_LIMITED` with `retryAfterMs`.
- **Exactly one `res` per `id`.**
- **`"noFence":true`** skips the read-your-writes fence (§6.2). Use it for latency-sensitive calls that do not write.
- **`"ik":"<uuid>"` idempotency key.** Negotiated with the `ik` cap: the client offers `"caps":["ik"]` in `hello`, and a server that supports keys includes `"ik"` in `welcome.caps`.
  - The framework writes `(ik, userId, method, result)` to `comet_idempotency` inside the method's own transaction, so the record commits atomically with the method's effects.
  - A retry with the same `ik` from the same user returns the stored result instead of executing again. Its `res` is fenced like any call's.
  - A duplicate that arrives while the first is still running waits for its outcome; the handler runs once.
  - A failed call stores nothing. The same `ik` for a different method is `BAD_REQUEST`. `ik` must be a non-empty string of at most 128 bytes.
  - Stored results expire after a server-configured TTL (default 24 h).
- **cancel.** `{"t":"cancel","id":12}` cancels the method's Go `context`. The server still sends exactly one `res`: either `CANCELED`, or the real result if the method had already committed. Cancel does **not** imply rollback of a committed call.
  - A cancel that arrives while the handler runs rolls back its transaction and gives `CANCELED`, whatever the handler then returns. Queries the handler runs with that `context` are interrupted.
  - A cancel that arrives after the handler returned is too late: the transaction commits and the real result is sent.
  - A cancel for an unknown or finished `id` is ignored. A dropped connection does not cancel a call; only `cancel` does.
  - The client sends `cancel` for a call it has sent and settles the call from the `res`, never locally, because only the server knows the outcome. A call it has not sent yet is dropped locally instead.

## 6. Consistency guarantees

### 6.1 Snapshot guarantees

1. **One `tx` never mixes snapshots.** All ops in one `tx`, across all its subs, come from a single committed Postgres snapshot.
2. **No partial transactions.** Each sub's state is always exactly its result at some committed snapshot.
3. **Monotonic per sub.** A sub never moves to an older snapshot.
4. **Read-your-writes,** per §6.2.

Consistency across subs holds within a `tx`. **[later]** With slow-observer detachment (§8.4), two subs on one connection may sit at different snapshots between `tx` messages, but only while one of them is detached.

Guarantees hold per server node. In a multi-node deployment, each connection is served entirely by one node.

### 6.2 Read-your-writes fence

**Contract:** when a call's promise resolves, its effects are already in the client store, unless `res` carries `"lag":true`.

**Mechanism.** Methods that use the framework transaction (`ctx.Tx`) send `pg_notify('comet', '1|f|<nodeId>:<counter>')` as the **last** statement before commit.

Postgres delivers a transaction's notifications together, after commit, in commit order. So by the time the listener receives the fence, every row notification from that transaction has already been received.

**Release rule.**

- **Notification numbering.** The listener assigns a monotonic number `n` to each notification it receives. The fence for a call on connection C is received at position `p`.
- **Observer freshness.** Each observer tracks `freshAsOf`: the highest `n` drained before the snapshot that produced its last delivered result.
- **Condition.** `res` is released for C when every observer bound to C's subscriptions satisfies one of:
  - it has no pending dirty mark from a notification with `n ≤ p`, or
  - its `freshAsOf ≥ p`, and that diff has been enqueued on C.
- **Scope.** Only C's own observers matter. Read-your-writes is a per-connection promise.

In v1-core, ticks are sequential and every dirty observer runs in each tick, so this reduces to sending `res` after the `tx` of the first tick that drained past `p`.

**Edge cases:**

| Situation | Behaviour |
|---|---|
| Fence timeout (default 5 s) | `res` is sent with `"lag":true`. The promise resolves, and the client can tell the effects are not yet visible. The guarantee is never broken silently. |
| Method failed or rolled back | The fence notify rolled back with it. The framework knows the commit failed and sends `res` immediately. |
| Method wrote nothing watched | The fence still forces a (cheap) tick. |
| Writes outside `ctx.Tx` (another connection or pool) | No fence guarantee. This is documented. |
| **[later]** logical replication | The fence becomes "the replication stream has processed past the commit LSN". The wire protocol is unchanged. |

## 7. Reconnect

### 7.1 Resync (v1-core)

A lost connection leads to a resync when session resume (§7.2) is not available or fails:

1. The client reconnects with exponential backoff and full jitter (base 500 ms, cap 30 s). It reconnects immediately on the `online` and `visibilitychange` events.
2. It sends `hello`, then re-sends `sub` for every active subscription with the **same `s`** and `"have":true`.
3. The server answers with `reset:true` entries. The client diffs and swaps at `ready` (§4.4), with no flicker.
4. Calls in flight without a `res` are rejected locally with `CONNECTION_LOST`, meaning "outcome unknown". They are **never** re-executed blindly. Calls carrying `ik` are resent on the new connection, with the same `ik`, after its `welcome`, if that `welcome` advertises the `ik` cap (§5) and names the same user the call ran as.

### 7.2 Session resume

Resume makes a short drop invisible: no snapshot is re-sent, and calls in flight are not lost. It applies when the same node, the same `epoch` and a grace window (default 60 s) all still hold.

**Negotiation.** The client sends `"caps":["resume"]` in `hello`; the server advertises `"caps":["resume"]` in `welcome`. The server keeps a session resumable only when both did. A client without the cap gets the v1-core behaviour and never needs to ack.

**Client acks.**
- The client acks with `{"t":"ack","seq":N}`, about every 1 s or 64 KiB, piggybacked on its liveness check. `N` is the last `seq` it has fully received.
- An ack for a `seq` not yet sent, or not above the previous ack, is ignored.
- The server keeps a replay buffer of the serialized stateful messages (`tx`, `res`, `authRes`) after the last ack, at most 1 MiB by default.
- If the buffer overflows while the client is connected, the oldest messages are dropped. A resume to a `seq` below the dropped ones is impossible, and becomes possible again once the client acks past them.
- When the socket is lost, the session is kept for the grace window. Observers stay bound, output keeps accumulating in the buffer, and in-flight calls and token expiry keep running. If the buffer overflows while the client is disconnected, the session ends at once.

**Resume request.**

```json
→ {"t":"hello","v":1,"caps":["resume"],"auth":{…},"resume":{"session":"s_9fK2…","seq":1841,"subs":[7,9]}}
```

`seq` is the last `seq` the client fully received, and `subs` lists every sub id it still holds.

**Validity.** The resume succeeds only if all of these hold:
- the session exists on this node and has not ended,
- `seq` is not below the last ack or the overflow point, and not above the last `seq` sent,
- the `hello` authenticates the same principal (same user ID, or anonymous on both sides) as the session's current identity. A refreshed token for the same user is accepted, and its expiry replaces the old one.

For an anonymous session, the session id is the only credential, as a session cookie would be. It is 72 random bits, is sent only in `welcome`, and must be kept as secret as a token.

A deliberate close (WebSocket close code 1000 or 1001, e.g. `Comet.close()` or a page unload) ends the session at once: that client will not resume.

**The server, on a valid resume:**
- Answers `welcome{resumed:true}` with the same `session`, then replays the buffered messages with `seq > 1841`, in order, before anything new. The resume `seq` also counts as an ack.
- If a previous socket of the session is still open (the client saw it die before the server did), closes it.
- Unbinds any bound sub not listed in `subs`. This covers unsubs lost in transit. No `end` is sent, because the client no longer holds those subs.
- Treats re-sent `sub` messages as no-ops (§4.1).
- Deduplicates re-sent `call`/`auth` messages by `id`:
  - if the call is still running, it is ignored, and its `res` arrives when it finishes,
  - if it already completed, its `res` is already in the replay.

The client re-sends its subs pipelined after `hello`, as in §7.1, but holds calls and auths sent before the drop until `welcome` arrives. It re-sends them with their original ids only on `resumed:true`. On `resumed:false` it rejects them with `CONNECTION_LOST`.

**Fallback.** If the session is gone (restart, different node, expired grace window, replay overflow), the `seq` is out of range, or the principal differs, the server answers `resumed:false` with a new `session`, and the client falls back to §7.1. A failed resume ends the old session, because its owner has given up on it.

An identity change during the gap is part of the replay. For example, a token that expires while the client is away produces the usual reset `tx` and `authRes{AUTH_EXPIRED}` (§3.2). The session is then anonymous, so only an anonymous `hello` can resume it.

**Multi-node.** Sessions live in one node's memory, so resume requires sticky routing: the reconnect must reach the node that served the session. A reconnect that lands on another node gets `resumed:false` and resyncs, which is correct, just more expensive.

### 7.3 Server shutdown

- **Graceful shutdown:**
  1. The server sends `{"t":"goaway","reason":"restart","retryInMs":<random 0–5000>}`. `goaway` is not stateful: it has no `seq` and is never replayed. New WebSocket upgrades get HTTP 503.
  2. It stops accepting calls and drains in-flight calls. A `call` that arrives after `goaway` gets `res` with `UNAVAILABLE` and `retryAfterMs` equal to `retryInMs`. A call already running completes, and its `res` (after its fence, §6.2) is sent before the close.
  3. Once no call is in flight, or the server's shutdown deadline passes, it closes the connection with code 4503, after writing everything queued for it.

  The client waits `retryInMs` before reconnecting, instead of its backoff. Calls made after `goaway` are not sent on the closing connection; they wait for the next one. Calls already sent settle from their `res` if it arrives before the close, and otherwise as on any drop (§7.1, §7.2). `goaway` needs no cap: a client that does not know it ignores it and reconnects with its backoff after the 4503.
- **Crash.** Clients reconnect using the backoff rules in §7.1.

## 8. Flow control, liveness, limits

### 8.1 Outbound

Each connection has a single writer goroutine with a 10 s write deadline.

- **Coalescing mode.** Above a 256 KiB soft limit (raised, until the queue next drains below the low-water mark, by whatever was queued right after the connection was last sent a snapshot: a queue full of a snapshot is not a client falling behind):
  - New diffs for the connection are not serialized. Instead its affected subs are marked `needsReset`. A sub made while coalescing is held the same way, and later gets its first snapshot (`init` without `reset`).
  - When the queue drains below the 64 KiB low-water mark, the server sends one `tx` with `reset:true` for all marked subs, built from a single tick. That snapshot is newer than anything the connection was sent, so no sub goes back in time.
  - A `res` whose fence is released while the connection is coalescing (§6.2), and an `authRes` after a rebind (§3.2), do not wait for the drain: the marked subs' resets go out in that tick's `tx`, ahead of the message.
  - `evt` messages for a coalescing connection are dropped (§4.5, at-most-once): for a marked sub they would arrive ahead of the data they follow, and queueing them would grow memory with the event rate.
  - A session that loses its socket while coalescing (§7.2) keeps its subs marked; the resets follow the replay once it resumes.
  - Server memory stays bounded by result size, not by change rate.
  - No cap is needed: clients already handle `reset:true` for resync.
- **Hard limit.** As a last resort, about 8 MiB queued, or a write making no progress for the 10 s deadline, closes the connection with 4429 SLOW_CONSUMER. The client reconnects and resyncs.

### 8.2 Heartbeats

Browsers cannot observe WebSocket control frames, so liveness is checked on both sides:

- **Server:** sends WebSocket ping frames every `hb`. The browser auto-pongs. The server closes with 4408 after `2 × hb` without a pong.
- **Client:**
  - sends `{"t":"ping"}` after `hb` ms with nothing received; the server answers `{"t":"pong"}`,
  - declares the connection dead after `hb + 10 s` of silence,
  - counts any inbound message as a sign of life.

### 8.3 Limits (defaults, advertised in `welcome.limits`)

| Limit | Default | Behaviour |
|---|---|---|
| `maxSubs` | 500 | |
| `maxInflight` | 32 | |
| `maxMsg` | 1 MiB | |
| Per-observer row cap | 10,000 | Sub ends with `RESULT_TOO_LARGE`. |
| Observer query hard timeout | 30 s | Sub ends with `TIMEOUT`. |
| **[later]** per-method rate limits | | Token bucket. |

### 8.4 [later] Slow observers

A query that exceeds a 1 s soft deadline is **detached**, not cancelled:

- It finishes on its own snapshot (an exported or dedicated REPEATABLE READ snapshot).
- Its diff goes out in its own `tx`.
- It is not re-run while it is in flight; notifications that arrive meanwhile keep it dirty.
- Ticks and fences (per the §6.2 rule) proceed without it.

Cancel-and-retry is rejected because a query that always exceeds the deadline would never finish.

## 9. Errors

**Shape everywhere:** `{"code":"…","msg":"…","details":{…}?,"retryAfterMs":N?}`.

**Stable codes:**

| Category | Codes |
|---|---|
| Request and auth | `BAD_REQUEST`, `VALIDATION` (`details.fields`), `UNAUTHENTICATED`, `AUTH_EXPIRED`, `FORBIDDEN` |
| Data | `NOT_FOUND`, `CONFLICT` |
| Load and timing | `RATE_LIMITED`, `TIMEOUT`, `CANCELED`, `RESULT_TOO_LARGE` |
| Server and connection | `UNAVAILABLE`, `INTERNAL` (`details.ref` is a log correlation ID; internals are never leaked), `CONNECTION_LOST` (synthesized by the client) |

Application-defined codes are namespaced `app.<name>`.

**Fatal errors** use WebSocket close codes, with a JSON reason `{code,msg}`:

| Close code | Meaning |
|---|---|
| 4400 | PROTOCOL |
| 4401 | UNAUTHENTICATED (cookie mode at handshake only) |
| 4408 | HEARTBEAT_TIMEOUT |
| 4429 | SLOW_CONSUMER |
| 4503 | GOING_AWAY |

## 10. Type mapping

Types are described once per sub in `init.cols`, and values are plain JSON. There is no per-value tagging. Method args and results use the same codes through Go→TypeScript codegen.

| Postgres | code | Wire (JSON) | TS decode (default) |
|---|---|---|---|
| int2, int4 | `i4` | number | number |
| int8, bigserial | `i8` | decimal **string** | `string` (per-column opt-in: `bigint`) |
| float4, float8 | `f8` | number; `"NaN"`, `"Infinity"`, `"-Infinity"` as strings | number |
| numeric | `num` | exact decimal string (`"NaN"` allowed) | string (pluggable Decimal) |
| bool | `bool` | bool | boolean |
| text, varchar, citext | `text` | string | string |
| enum | `enum` | string | string (TS union via codegen). User-defined enums are detected from the catalog; arrays of them (or of a domain over one) are `[]enum`. |
| uuid | `uuid` | lowercase canonical string | string |
| timestamptz | `tstz` | RFC 3339 UTC, microsecond precision, `Z`; `"infinity"`/`"-infinity"` | `Date` (opt-in: `Temporal.Instant` or raw string; `Date` drops µs) |
| date | `date` | `"YYYY-MM-DD"` | string |
| json, jsonb | `json` | inline JSON value (not double-encoded) | `unknown` / typed via codegen. Numbers above 2^53 inside jsonb lose precision (documented) |
| bytea | `bytes` | standard padded base64 | `Uint8Array` |
| 1-D arrays | `[]<code>` | JSON array, `null` elements allowed | array of the element decode |
| NULL | any | `null` | `null` |
| **[later]** timestamp, time, timetz, interval, inet/cidr/macaddr, multi-dimensional arrays, range, composite | `ts`, `time`, `timetz`, `ivl`, `inet`, `raw`… | ISO forms / Postgres text form | string |

**Client→server encoding:** `bigint` → decimal string, `Date` → RFC 3339, `Uint8Array` → base64. Go decodes into the method's declared parameter struct.

**[later] CBOR:** native int64 and byte strings, tags 1/1001 for timestamps, tag 4 for decimals.

## 11. Postgres change feed (server-internal, v1)

Not part of the wire protocol. Specified here because the consistency guarantees depend on it.

All notifications use channel `comet`. Payload fields are separated by `|`, and the JSON part is always last.

| Kind | Payload | Example |
|---|---|---|
| Rows changed | `1\|r\|<I\|U\|D>\|<schema.table>\|<json array of pk arrays>` | `1\|r\|U\|public.tasks\|[[42],[43]]`<br>composite: `[["org_1",42]]` |
| Table dirty | `1\|t\|<I\|U\|D\|T>\|<schema.table>` | |
| Fence | `1\|f\|<nodeId>:<counter>` | |

**Row notifications:**
- Emitted by **statement-level AFTER triggers with transition tables** (PG 10+), one trigger per event (INSERT, UPDATE, DELETE), because a transition-table trigger cannot list several events.
- Each builds `json_agg(json_build_array(pk…))` from the transition table and chunks the payload to at most 7,500 bytes (the hard limit is 8,000).
- An UPDATE reports the union of old and new PKs, so primary-key changes are handled.
- PK values use the wire encoding (int8 as text).

**Table-dirty notifications** are emitted when a statement touches more than 1,000 rows, for TRUNCATE (`T`, via a statement trigger), or when a PK set would not fit.

**Observer invalidation:**
- Observers declare the tables they read. Any notification for one of those tables marks the observer dirty.
- A dirty observer re-runs its full query. An observer whose query declares a single-table equality predicate (`Query.Match` on the server) is dirtied by a row notification only if a changed row, read once per tick by PK inside the snapshot, matches the predicate, or its current result holds a changed PK. Table-dirty notifications and anything the server cannot evaluate re-run it as before.
- A minimum interval between ticks (default 20 ms, `Config.MinTickInterval`) coalesces bursts.

**Listener connection loss.** Notifications sent while the LISTEN connection is down are lost. On reconnect, the server marks **all** observers dirty.

Every node receives every fence and ignores the tokens it does not own.

## 12. Scope

**v1-core** (Go server + TypeScript client + todo demo):
- **Connection:** subprotocol `cometgo.1.json`; `hello`/`welcome` (token or cookie auth, `epoch`, `build`); `ping`/`pong`; error shapes and close codes.
- **Subscriptions:** `sub`/`unsub`/`have`; `tx` with `init`/`ops`/`reset`/`ready`/`end`/`more`; resync on reconnect.
- **Methods:** `call`/`res` with the fence and `lag`; `CONNECTION_LOST`; `maxInflight`.
- **Auth:** `auth`/`authRes`, with identity change implemented as reset of all subs in one `tx`.
- **Server engine:**
  - shared observers keyed by `(pub, args, scopeKey)`
  - full query re-runs
  - sequential REPEATABLE READ ticks
  - hard query timeout
  - the §11 change feed
- **Types:** `i4 i8 f8 num bool text enum uuid tstz date json bytes []<code>`.
- **Backpressure:** hard limit only (4429).

**Implemented since v1-core**:
- **Reconnect:** session resume (`seq`/`ack`/replay, §7.2), behind `caps`.
- **Events:** `evt` (ephemeral, at-most-once, sub-scoped, §4.5), behind `caps`.
- **Methods:** `cancel` (§5). It needs no cap: a server without it ignores the message, and the call settles from its normal `res`.
- **Methods:** `ik` idempotency keys (§5), behind `caps`.
- **Engine:** changed-PK filtering with Go-side equality predicates (§11). Server-internal; no wire change.
- **Reconnect:** `goaway` graceful shutdown (§7.3). It needs no cap: clients ignore unknown message types.
- **Backpressure:** coalesce→reset for slow clients (§8.1). It needs no cap: it only sends `reset` entries, which every client handles.

**[later]**, all additive and behind `caps`:
- **Engine and transport:**
  - slow-observer detachment and parallel ticks with an exported snapshot
  - minimal-diff identity rebind
  - logical replication
- **Client and features:**
  - presence
  - hot reload via `build`
  - `src` interning (safe only between subs last updated in the same `tx`)
  - `sub{replaceOf}` for changing args without a second snapshot
  - digest-skip on resync
  - CBOR
  - extended types
  - optimistic UI (client stubs keyed by call `id`, rolled back on `res`)

## 13. What this fixes relative to DDP

1. Per-connection mergebox (O(connections × docs)) is replaced by shared observers with per-sub results.
2. Half-applied multi-row updates are replaced by atomic per-snapshot `tx` messages.
3. Separate `result` and `updated` messages are replaced by one `res`, ordered after its data.
4. Blind method replay on reconnect is replaced by `CONNECTION_LOST` and idempotency keys.
5. Resubscribe-with-flicker is replaced by diff-based `reset` and seq/ack resume.
6. EJSON per-value tagging, which had no int64 or decimal story, is replaced by column type descriptors.
7. Client-side re-sorting with the wrong collation is replaced by server positions.
8. Login as a method with non-atomic sub reruns is replaced by first-class `auth` with an atomic rebind.
9. No backpressure is replaced by coalesce→reset, with a hard limit as the last resort.
10. In-payload version negotiation is replaced by the WebSocket subprotocol.

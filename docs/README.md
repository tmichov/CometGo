# CometGo

CometGo is a realtime web framework for Go, inspired by MeteorJS. A Go server serves your UI and keeps the browser in sync with Postgres over a single WebSocket. A small TypeScript client holds the live data in a reactive store.

These docs describe **v1-core**, the part of the [wire protocol](protocol.md) that is implemented today. Features the protocol specifies but v1-core does not implement yet are marked *planned*; the full list is in [protocol §12](protocol.md#12-scope).

## The mental model

CometGo has two building blocks.

**Publications are live Postgres queries.** The server registers a named query with `cometgo.Publish`. A client subscribes to it by name, gets the full result as an initial snapshot, and then receives diffs (insert, update, delete, move) whenever the tables the query reads change. All changes produced by one database snapshot reach the client as one atomic message, so the UI never shows half of a transaction. Postgres triggers installed by `cometgo.InstallTriggers` tell the server which tables changed.

**Methods are server-authoritative writes.** The server registers a named function with `cometgo.Method`. The client calls it with `comet.call(name, args)`. The method runs in a Postgres transaction it gets from the framework, and the client never simulates the write locally. When the call's promise resolves, the effects of that write are already in the client's subscriptions (*read-your-writes*). If that takes longer than a timeout, the result still arrives and is flagged as lagging rather than silently breaking the guarantee.

```
browser                                  Go server                       Postgres
───────                                  ─────────                       ────────
subscribe("todos.all") ───── sub ─────►  observer runs the query ──────► SELECT …
rows ◄────────────────────── tx ───────  snapshot + diffs
call("todos.add") ────────── call ────►  method runs in c.Tx ──────────► INSERT … COMMIT
                                         trigger → NOTIFY ◄───────────── change feed
rows (new row) ◄──────────── tx ───────  re-run, diff
promise resolves ◄────────── res ──────  (sent after the tx above)
```

## Pages

- [Getting started](getting-started.md): run the todo example and see how it is built.
- [Server](server.md): the Go API (`Config`, `Publish`, `Method`, errors, auth, `InstallTriggers`).
- [Client](client.md): the TypeScript API (`connect`, subscriptions, calls, auth, reconnects, type decoding).
- [Typed clients](codegen.md): generate TypeScript types for methods and publications from the Go server.
- [Wire protocol](protocol.md): the full protocol specification, including planned features.
- [Load benchmarks](bench/README.md): the load harness and the recorded baseline numbers.
- [M3 design: slow-work isolation](design/m3-slow-work.md): the engine design for detaching slow queries and slow publication `fn` calls, and per-connection fences, with how it was verified.
- [M4 design: the logical-replication change feed](design/m4-logical-feed.md): the agreed, not yet implemented, design for replacing NOTIFY with logical decoding, with the measurements behind it.
- [M5 design: hosted, multi-tenant and multi-node foundations](design/m5-hosted.md): the hosted product's shape, tenant routing and metrics, and the conditional hub fan-out design, with the first milestone's exit criterion.
- [Known limitations](limitations.md): what the current engine does not do well, and when that matters.
- [Stability policy](stability.md): versioning, and what may change before 1.0.

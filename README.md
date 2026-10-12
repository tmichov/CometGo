# CometGo

> **Preview (v0.1.0).** CometGo works end to end, but it is young: read the [known limitations](docs/limitations.md) before relying on it, and expect the API to change before 1.0 ([stability policy](docs/stability.md)).

A realtime web framework for Go, inspired by MeteorJS.

- **The Go server serves your UI** and pushes live data over WebSocket.
- **Postgres live queries:** subscribe to a query, get an initial snapshot, then atomic diffs as the database changes. A client never sees half a transaction.
- **Server-authoritative methods with read-your-writes:** when `await call()` resolves, its effects are already in the client's store.
- **TypeScript client** with a reactive store. Unchanged rows keep their object identity, and reconnects don't flicker.

## Quick start

### Try the todo example

Requires Go 1.25+, Node 22.18+ and Docker.

```sh
git clone https://github.com/tmichov/CometGo.git && cd CometGo
make db      # Postgres on localhost:54329
npm install
make todo    # builds the UI and serves it at http://localhost:8080
```

Open the page in two tabs: changes appear in both. [Getting started](docs/getting-started.md) walks through how the example is built.

### Add it to your app

```sh
go get github.com/tmichov/cometgo@v0.1.0   # the server
npm install @cometgo/client                 # the browser client (React hooks: @cometgo/client/react)
```

Then follow the [server](docs/server.md) and [client](docs/client.md) guides.

## Documentation

- [Overview](docs/README.md)
- [Getting started](docs/getting-started.md): the todo example, step by step
- [Server](docs/server.md): the Go API
- [Client](docs/client.md): the TypeScript API
- [Wire protocol](docs/protocol.md)
- [Known limitations](docs/limitations.md)
- [Stability policy](docs/stability.md)

## Status

v1-core is implemented (see [protocol §12](docs/protocol.md#12-scope)), plus session resume, idempotency keys, ephemeral events and coalescing backpressure. Optimistic UI is specified but not implemented yet. Before using it, read the [known limitations](docs/limitations.md); the API may still change before 1.0 ([stability policy](docs/stability.md)).

## License

MIT

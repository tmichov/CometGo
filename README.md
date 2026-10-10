# CometGo

A realtime web framework for Go, inspired by MeteorJS.

- **The Go server serves your UI** and pushes live data over WebSocket.
- **Postgres live queries:** subscribe to a query, get an initial snapshot, then atomic diffs as the database changes. A client never sees half a transaction.
- **Server-authoritative methods with read-your-writes:** when `await call()` resolves, its effects are already in the client's store.
- **TypeScript client** with a reactive store. Unchanged rows keep their object identity, and reconnects don't flicker.

The wire protocol is specified in [PROTOCOL.md](PROTOCOL.md).

## Quick start

Requires Go 1.26+, Node 22+ and Docker.

```sh
make db      # Postgres on localhost:54329
npm install
make todo    # builds the UI and serves it at http://localhost:8080
```

Open the page in two tabs: changes appear in both.

## Server

```go
app := cometgo.New(cometgo.Config{DB: pool})
cometgo.InstallTriggers(ctx, pool, "todos") // change feed, idempotent

cometgo.Publish(app, "todos.all",
	cometgo.PubOptions{Tables: []string{"todos"}, Scope: cometgo.ScopeShared},
	func(c *cometgo.SubCtx, _ struct{}) (cometgo.Query, error) {
		return cometgo.Query{SQL: `SELECT id, title, done FROM todos ORDER BY id`}, nil
	})

cometgo.Method(app, "todos.add", func(c *cometgo.Call, a struct{ Title string }) (string, error) {
	var id string
	err := c.Tx.QueryRow(c.Context, `INSERT INTO todos (title) VALUES ($1) RETURNING id::text`, a.Title).Scan(&id)
	return id, err
})

app.Start(ctx)
http.Handle("/_comet", app)
```

## Client

```ts
import { connect } from "@cometgo/client";

const comet = connect();
const todos = comet.subscribe<Todo>("todos.all");
todos.subscribe(() => render(todos.rows)); // also works with React's useSyncExternalStore

await comet.call("todos.add", { Title: "Ship v1" }); // the new row is already in todos.rows
```

## Development

```sh
make test   # go vet, go test, tsc, client tests
COMETGO_TEST_DATABASE_URL=postgres://comet:comet@localhost:54329/comet go test ./...   # with integration tests
COMETGO_WRITE_VECTORS=1 go test -run TestWriteDiffVectors .   # regenerate client diff vectors
```

## Status

v1-core is implemented (see PROTOCOL.md §12). Session resume, idempotency keys, coalescing backpressure, ephemeral events and optimistic UI are specified but not implemented yet.

## License

MIT

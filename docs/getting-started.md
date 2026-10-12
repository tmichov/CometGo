# Getting started

## Prerequisites

- Go 1.26 or newer
- Node 22.18 or newer (to build the example UI and run the client tests; the tests run `.ts` files directly, which needs Node's built-in type stripping)
- Docker (for the local Postgres)

## Run the todo example

```sh
make db       # starts Postgres 17 on localhost:54329 (user, password and database: comet)
npm install   # esbuild and TypeScript
make todo     # bundles the UI, then runs go run ./examples/todo
```

Open <http://localhost:8080> in two tabs. Adding, ticking or deleting a todo in one tab shows up in the other.

The example reads two environment variables:

| Variable | Default |
|---|---|
| `DATABASE_URL` | `postgres://comet:comet@localhost:54329/comet` |
| `ADDR` | `:8080` |

## How the example is built

The server is [`examples/todo/main.go`](../examples/todo/main.go). The browser code is [`examples/todo/web/src/main.ts`](../examples/todo/web/src/main.ts), which `npm run build` bundles into `web/public/app.js`. The Go binary embeds `web/public` and serves it.

### 1. Schema and change feed

```go
pool, err := pgxpool.New(ctx, dsn)
pool.Exec(ctx, schema) // CREATE TABLE IF NOT EXISTS todos (id bigserial PRIMARY KEY, …)
cometgo.InstallTriggers(ctx, pool, "todos")
```

`InstallTriggers` adds the triggers that notify CometGo when rows in `todos` change. It is idempotent, so running it at every startup is fine. Every table a publication reads needs it, and the table must have a primary key. See [Server: InstallTriggers](server.md#installtriggers).

### 2. The app

```go
app := cometgo.New(cometgo.Config{DB: pool})
```

Everything else in `Config` has a default. See [Server: Config](server.md#config).

### 3. A publication

```go
cometgo.Publish(app, "todos.all", cometgo.PubOptions{Tables: []string{"todos"}, Scope: cometgo.ScopeShared},
	func(c *cometgo.SubCtx, _ struct{}) (cometgo.Query, error) {
		return cometgo.Query{SQL: `SELECT id, title, done, created_at FROM todos ORDER BY created_at, id`}, nil
	})
```

- `Tables` lists every table the query reads. A change to any of them re-runs the query.
- The result's key column defaults to `id`.
- `ScopeShared` is correct here only because the demo has no users: every visitor sees the same list, so all connections can share one copy of the query. Any query that depends on who is asking must keep the default `ScopeUser`. See [Server: Scope](server.md#scope).
- `ORDER BY` makes the order deterministic. The client never sorts; it shows rows in the order the server sends.

### 4. Methods

```go
type addArgs struct {
	Title string `json:"title"`
}

cometgo.Method(app, "todos.add", func(c *cometgo.Call, a addArgs) (added, error) {
	title := strings.TrimSpace(a.Title)
	if title == "" {
		return added{}, cometgo.ValidationError(map[string]string{"title": "required"})
	}
	var out added
	err := c.Tx.QueryRow(c.Context, `INSERT INTO todos (title) VALUES ($1) RETURNING id::text`, title).Scan(&out.ID)
	return out, err
})
```

- The client's args are decoded into `addArgs`. Unknown fields are rejected with `BAD_REQUEST`.
- `ValidationError` sends a `VALIDATION` error with per-field messages that the UI can show.
- The write goes through `c.Tx`. That is what makes the client's `await call(...)` resolve only after the new row is in its subscription.
- `id` is a `bigint`, which travels as a decimal string (JavaScript numbers cannot hold every int8), so the method returns it as text and `idArgs` takes it as a `string`.

`todos.toggle` shows returning a `NOT_FOUND` error with `cometgo.NewError` when no row matched.

### 5. Serving

```go
if err := app.Start(ctx); err != nil {
	log.Fatal(err)
}

mux := http.NewServeMux()
mux.Handle("/_comet", app)               // the WebSocket endpoint
mux.Handle("/", http.FileServerFS(public)) // the UI
```

`Start` runs the Postgres listener and the engine until `ctx` is cancelled. `App` is an `http.Handler`; mount it at `/_comet`, which is where the client connects by default.

### 6. The client

```ts
import type { Schema } from "./schema.ts";

const comet = connect<Schema>();
const todos = comet.subscribe("todos.all");
todos.subscribe(render);

const { lag } = await comet.callWithStatus("todos.add", { title });
```

`schema.ts` is generated from the Go server with `make gen`, so a misspelled method or wrong args fail `npm run typecheck` (see [Typed clients](codegen.md)). `connect()` with no options connects to `/_comet` on the page's own host. `todos.rows` always holds the current rows and `todos.subscribe` fires after each change. Because the call resolves only after its effects arrive, `render` has already run with the new row by the time the `await` returns. `created_at` is a `timestamptz`, so it arrives as a `Date`. See [Client](client.md).

## Tests

```sh
make test   # go vet, go test, tsc, client tests
COMETGO_TEST_DATABASE_URL=postgres://comet:comet@localhost:54329/comet go test ./...   # with integration tests
COMETGO_WRITE_VECTORS=1 go test -run TestWriteDiffVectors .   # regenerate the client's diff test vectors
```

The integration tests need the database from `make db` and are skipped without `COMETGO_TEST_DATABASE_URL`.

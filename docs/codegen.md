# Typed clients

The Go server knows the argument and result type of every method, and the columns of every publication's rows. `cometgo.Describe` reads them, and `Schema.TypeScript()` writes them out as a TypeScript module. Pass the module's `Schema` type to `connect` and the client checks method names, args, results, publication names and row fields at compile time.

```ts
import { connect } from "@cometgo/client";
import type { Schema } from "./schema.ts"; // generated

const comet = connect<Schema>();
const todos = comet.subscribe("todos.all");          // rows: { id: string; title: string; done: boolean; created_at: Date }[]
const { id } = await comet.call("todos.add", { title: "Ship v1" });
comet.call("todos.ad", { title: "x" });              // error: unknown method
comet.call("todos.add", { name: "x" });              // error: wrong args
```

## Generating the schema

`Describe` needs the registered app and its database. Most apps add a flag to their own binary that describes the app and writes the file, as the todo example does ([`examples/todo/main.go`](../examples/todo/main.go)):

```go
s, err := cometgo.Describe(ctx, app, cometgo.DescribeOptions{})
if err != nil {
	return err
}
return os.WriteFile("web/src/schema.ts", s.TypeScript(), 0o644)
```

| Identifier | Description |
|---|---|
| `Describe(ctx, app, DescribeOptions) (*Schema, error)` | Describes every registered method and publication. Changes nothing in the database. |
| `DescribeOptions.Identity *Identity` | The identity publication builders see. Nil means anonymous; set it when a builder refuses anonymous callers. |
| `(*Schema) TypeScript() []byte` | The generated module: `interface Methods`, `interface Publications` and `interface Schema`. |
| `Schema.Methods`, `Schema.Publications` | The same information as Go values, for other generators. |

The output is sorted by name, so it changes only when the server does. To check in CI that the committed file is current, compare it with a fresh `TypeScript()` and fail when they differ. The todo example does this with `go run . -gen -check` (`make gen-check`) and with `TestSchemaUpToDate`, which runs under `go test ./...` when `COMETGO_TEST_DATABASE_URL` is set. Run `make gen` to regenerate it.

## Methods

Method args and results are typed from the Go types given to `Method`, following `encoding/json`:

| Go | TypeScript |
|---|---|
| `bool` | `boolean` |
| integer and float types, `json.Number` | `number` |
| `string` | `string` |
| field tagged `,string` (number, bool or string) | `string` |
| `time.Time` | `string` (RFC 3339) |
| `[]byte` | `string \| null` (base64) |
| `[]T` | `T[] \| null` |
| `[N]T` | `T[]` |
| `map[K]V` | `Record<string, V> \| null` |
| `*T` | `T \| null` |
| struct | an object type of its encoded fields |
| `struct{}` | `Record<string, never>` |
| a type with `MarshalText` | `string` |
| a type with `MarshalJSON`, `json.RawMessage`, interfaces | `unknown` |

Struct fields follow `encoding/json`: `json` tags rename or drop them, `omitempty` and `omitzero` make them optional, embedded structs are promoted, and a field hidden by another of the same name, or reached through the same embedded type twice, is left out. A named type that contains itself (`type Tree map[string]Tree`) becomes `unknown` where it repeats.

Results are marshaled from the value the method returns, which `encoding/json` cannot take the address of. A `MarshalText` or `MarshalJSON` defined on `*T` therefore applies to a `T` only under a pointer or inside a slice, as in `encoding/json`; elsewhere `T` is typed by its fields. Args are decoded into an addressable value, so `UnmarshalText` (a `string`) and `UnmarshalJSON` (`unknown`) on `*T` always apply.

Args of `struct{}` may be left out: `comet.call("todos.clearDone")`.

Integers are `number` unless tagged `,string`. Values beyond 2^53 lose precision in JavaScript, so tag `int64` IDs with `,string` or carry them as strings.

## Publications

A publication's row type comes from its result columns. `Describe` calls the publication's builder with zero-value args, runs the query wrapped as `SELECT * FROM (<query>) q LIMIT 0`, and reads the column types. They decode as on the [client](client.md#type-decoding):

| Column type code | TypeScript |
|---|---|
| `i4`, `f8` | `number` |
| `i8`, `num`, `text`, `uuid`, `date`, `ts`, `raw` | `string` |
| `bool` | `boolean` |
| `tstz` | `Date` |
| `bytes` | `Uint8Array` |
| `json` | `unknown` |
| `enum` | the union of its labels, e.g. `"sad" \| "ok" \| "happy"` |
| `[]<code>` | `(<type> \| null)[]` |

A `timestamptz` of `infinity` or `-infinity` arrives as that string, not as a `Date`. The generated type does not include that case.

**Nullability.** Postgres does not say whether a result column can be null. A column is typed non-null when:

- it is one of the publication's `Key` columns (a row with a null key fails the query, so a delivered row never has one), or
- the publication's `Tables` lists exactly one table, the column comes straight from a `NOT NULL` column of that table, and the query's plan (`EXPLAIN`) has no outer join and no grouping sets (`ROLLUP`, `CUBE`, `GROUPING SETS`), either of which can put null in such a column.

Every other column, including expressions, aggregates and the non-key columns of a multi-table publication, is typed `T | null`. To state the types by hand, use `Row`.

If the builder, or the encoding of its `Query.Args`, fails or panics with zero-value args, `Describe` returns an error naming the publication (a panic includes its stack trace). Set `Row` for such a publication.

**`PubOptions.Row`.** A query that cannot run with zero-value args, or whose types should be stated by hand, can set `Row` to a struct value. `Describe` then uses the struct and does not run the query. Each field is a column, named by its `json` tag or field name. A pointer field is nullable. A `cometgo` tag sets the column's type code directly (`cometgo:"date"`, `cometgo:"[]enum"`); otherwise the Go type picks it:

| Go field | Column |
|---|---|
| `bool` | `bool` |
| `int8`, `int16`, `int32`, `uint8`, `uint16` | `i4` |
| `int`, `int64`, `uint`, `uint32`, `uint64` | `i8` |
| `float32`, `float64` | `f8` |
| `string`, or a type with `MarshalText` (UUID types, `netip.Addr`) | `text` |
| `time.Time` | `tstz` (tag `date` or `ts` columns) |
| `[]byte` | `bytes` (a byte array such as `[16]byte` needs a tag) |
| `[]T` | `[]` of `T`'s column type |
| `json.RawMessage`, maps, structs, interfaces | `json` |

```go
type taskRow struct {
	ID    int64      `json:"id"`
	Title string     `json:"title"`
	Due   *time.Time `json:"due"`
}

cometgo.Publish(app, "tasks.byProject", cometgo.PubOptions{Tables: []string{"tasks"}, Row: taskRow{}}, build)
```

A builder that refuses anonymous callers can be described with `DescribeOptions.Identity` instead.

## Client side

```ts
function connect(opts?: ConnectOptions): Comet
function connect<S extends Schema>(opts?: ConnectOptions): TypedComet<S>
```

`connect<Schema>()` returns the same `Comet`, typed as `TypedComet<Schema>`: `call`, `callWithStatus` and `subscribe` take only the schema's names, and check args and results. Everything else on `Comet` is unchanged. `connect()` without a type argument is untyped, as before.

The helper types `MethodName<S>`, `MethodArgs<S, M>`, `MethodResult<S, M>`, `PubName<S>`, `PubArgs<S, P>`, `PubRow<S, P>` and `CallRest<A>` are exported for wrappers that pass a call through:

```ts
async function run<M extends MethodName<Schema>>(method: M, ...rest: CallRest<MethodArgs<Schema, M>>) {
  const { lag } = await comet.callWithStatus(method, ...rest);
}
```

For React, `hooksFor<Schema>()` returns `useComet`, `useSubscription` and `useCall` typed against the schema. See [React bindings](react.md#typed-hooks).

# @cometgo/client

TypeScript client for [CometGo](https://github.com/tmichov/CometGo), a Go server that streams live Postgres query results to the browser over a WebSocket.

It has no runtime dependencies and ships as ES modules with type declarations. It has two entry points:

- `@cometgo/client`: connect, subscribe to publications, call methods. See the [client guide](https://github.com/tmichov/CometGo/blob/main/docs/client.md).
- `@cometgo/client/react`: React hooks over the same client. React 18 or 19 is an optional peer dependency. See the [React guide](https://github.com/tmichov/CometGo/blob/main/docs/react.md).

```ts
import { connect } from "@cometgo/client";

const comet = connect();
const todos = comet.subscribe<Todo>("todos.all");
todos.subscribe(() => render(todos.rows));
```

The package is ESM-only. Its type declarations are tested with TypeScript 4.9 and 5.x.

MIT licensed.

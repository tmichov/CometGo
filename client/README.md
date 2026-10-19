# @cometgo/client

TypeScript client for [CometGo](https://github.com/tmichov/CometGo), a Go server that streams live Postgres query results to the browser over a WebSocket.

It has no runtime dependencies and ships as ES modules with type declarations. It has three entry points:

- `@cometgo/client`: connect, subscribe to publications, call methods. See the [client guide](https://github.com/tmichov/CometGo/blob/main/docs/client.md).
- `@cometgo/client/react`: React hooks over the same client. React 18 or 19 is an optional peer dependency. See the [React guide](https://github.com/tmichov/CometGo/blob/main/docs/react.md).
- `@cometgo/client/react-native`: `connectNative`, the client for React Native, which reconnects promptly when the app returns to the foreground or the network changes. It needs Expo SDK 53 / React Native 0.79 or later (Metro must resolve package `exports`) and the optional peer dependencies `react-native`, `@react-native-community/netinfo` and `react-native-get-random-values`. It does not configure NetInfo: reachability checks, and their settings, stay the app's.

```ts
import { connect } from "@cometgo/client";

const comet = connect();
const todos = comet.subscribe<Todo>("todos.all");
todos.subscribe(() => render(todos.rows));
```

The package is ESM-only. Its type declarations are tested with TypeScript 4.9 and 5.x.

MIT licensed.

# React bindings

`@cometgo/client/react` wraps the [client](client.md) in React hooks. React 18 or 19 is a peer dependency; it is not bundled.

```tsx
import { connect } from "@cometgo/client";
import { CometProvider, useCall, useConnection, useSubscription } from "@cometgo/client/react";

const comet = connect();

createRoot(el).render(
  <CometProvider client={comet}>
    <Todos />
  </CometProvider>,
);

function Todos() {
  const { status } = useConnection();
  const todos = useSubscription<Todo>("todos.all");
  const call = useCall();
  if (todos.status === "loading") return <p>Loading…</p>;
  return (
    <ul className={todos.status === "resyncing" ? "stale" : ""}>
      {todos.rows.map((t) => (
        <li key={t.id} onClick={() => call("todos.toggle", { id: t.id })}>{t.title}</li>
      ))}
    </ul>
  );
}
```

The todo example has a React version at `/react.html` ([`examples/todo/web/src/react-main.tsx`](../examples/todo/web/src/react-main.tsx)), next to the plain TypeScript one at `/`.

## CometProvider

```ts
function CometProvider(props: { client: Comet; children?: ReactNode }): ReactNode
```

Makes `client` available to the hooks below. Every hook throws if it is used outside a provider. Subscriptions are shared within one provider (see below), so render one provider per `Comet`, near the root.

## useComet

```ts
function useComet(): Comet
```

Returns the provider's `Comet`, for anything the hooks do not cover (`setToken`, `callWithStatus`, `close`).

## useSubscription

```ts
function useSubscription<T>(name: string, args?: unknown): SubState<T>

interface SubState<T> {
  rows: readonly T[];
  status: SubStatus; // "loading" | "ready" | "resyncing" | "ended"
  error?: CometError;
}
```

Subscribes to a publication while the component is mounted and re-renders when its rows or status change. `rows`, `status` and `error` mean the same as on a [`Subscription`](client.md#subscriptions). The returned object, and `rows` within it, keep their identity until something actually changes, so they are safe to use as `useMemo`/`useEffect` dependencies.

- **Shared subscriptions.** Components under one provider that subscribe to the same `name` and `args` share one subscription on the wire. It is stopped (an `unsub` is sent) once the last of them unmounts.
- **Args are compared by value.** `{ page: 1, size: 10 }` and `{ size: 10, page: 1 }` are the same subscription, so passing a fresh object literal on every render does not resubscribe.
- **Changing args does not flash.** When `args` change, the hook subscribes to the new args but keeps returning the old rows until the new subscription is ready (or ends with an error), then switches in one render and stops the old one. Pagination and filters therefore never show an empty list in between. While it waits, `status` is the old subscription's (normally `"ready"`).
- **StrictMode-safe.** Stopping is deferred by one macrotask, so React's development double-mount (and any unmount followed by a remount of the same subscription) reuses the subscription instead of sending `unsub` and `sub` again.

A subscription the server ended (for example with `FORBIDDEN`) stays ended while any component still uses it. It is re-requested only after every component using it has unmounted.

## useConnection

```ts
function useConnection(): { status: ConnectionStatus; user: unknown }
```

The connection status (`"connecting" | "connected" | "disconnected"`) and the authenticated user (`null` when anonymous), re-rendering when either changes.

## useCall

```ts
function useCall(): Comet["call"]
```

Returns a stable function with the same signature as [`comet.call`](client.md#calls): `call(method, args?, { noFence? })`. Its identity does not change between renders, so it can go in dependency arrays. The promise resolves after the method's writes are already in this client's subscriptions, so by the time `await call(...)` returns, the components using them have the new rows.

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

Makes `client` available to the hooks below. Every hook throws if it is used outside a provider. Render it near the root. Subscriptions are shared per `Comet` (see below), so several providers for the same client share them too.

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

- **Shared subscriptions.** Components using the same `Comet` that subscribe to the same `name` and `args` share one subscription on the wire. A component that mounts while that subscription is already ready gets its rows on its first render. The subscription is stopped (an `unsub` is sent) once the last of them unmounts.
- **Args are compared by value.** `{ page: 1, size: 10 }` and `{ size: 10, page: 1 }` are the same subscription, so passing a fresh object literal on every render does not resubscribe.
- **Changing args keeps the old rows.** When `args` change, the hook subscribes to the new args but keeps returning the old rows until the new subscription is ready or ends, then switches in one render and stops the old one. Paging through results does not blank the list in between. While it waits, `status` is the old subscription's (normally `"ready"`), and there is no separate flag saying the rows belong to the previous args.
- **StrictMode-safe.** Stopping is deferred by one macrotask, so React's development double-mount (and any unmount followed by a remount of the same subscription) reuses the subscription instead of sending `unsub` and `sub` again.

A subscription the server ended (for example with `FORBIDDEN`) stays ended for the components already showing it. A component that mounts afterwards, or one whose args change to it, subscribes again, so after logging in, remounting the page is enough to retry.

If a component is hidden by `<Activity>` or its provider gets a different `client`, its subscription is released. When it shows again it starts from `"loading"` rather than showing the stopped subscription's rows.

## useConnection

```ts
function useConnection(): { status: ConnectionStatus; user: unknown }
```

The connection status (`"connecting" | "connected" | "disconnected"`) and the authenticated user (`null` when anonymous), re-rendering when either changes.

## useCall

```ts
function useCall(): Comet["call"]
```

Returns a stable function with the same signature as [`comet.call`](client.md#calls): `call(method, args?, { noFence?, signal?, idempotent?, ik? })`. Its identity does not change between renders, so it can go in dependency arrays.

As with `comet.call`, the promise normally resolves after the method's writes are already in this client's subscriptions. If they did not arrive within the server's fence timeout, it resolves anyway, and `call` does not tell you. Use `useComet().callWithStatus(...)` when you need the `lag` flag.

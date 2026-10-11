// React bindings for the CometGo client: @cometgo/client/react.

import {
  createContext,
  createElement,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import type {
  Comet,
  CometError,
  ConnectionStatus,
  PubArgs,
  PubName,
  PubRow,
  Schema,
  SubRest,
  Subscription,
  SubStatus,
  TypedComet,
} from "./index.ts";

// ---- shared subscriptions ----

interface Entry {
  key: string;
  sub: Subscription<any>;
  refs: number;
  stopTimer?: ReturnType<typeof setTimeout>;
  stopped?: boolean; // sub.stop() was called; it will never update again
  snap?: SubState<any>;
}

/** One reference to a shared subscription, held by one hook instance. */
interface Lease {
  registry: Registry;
  entry: Entry;
  released: boolean;
}

// Subscriptions are shared per Comet and ref-counted by (name, canonical
// args). The last release stops the sub only after a macrotask, so a
// StrictMode unmount/remount (or a remount on the same key) re-acquires the
// same sub instead of sending unsub + sub.
class Registry {
  readonly comet: Comet;
  #entries = new Map<string, Entry>();

  constructor(comet: Comet) {
    this.comet = comet;
  }

  /** The live entry for key, if any, without taking a reference. */
  peek(key: string): Entry | undefined {
    const e = this.#entries.get(key);
    return e && e.sub.status !== "ended" ? e : undefined;
  }

  acquire(key: string, name: string, args: unknown): Lease {
    let e = this.#entries.get(key);
    // A sub the server ended stays ended for its current holders, but a new
    // acquire (e.g. after login) asks again.
    if (!e || e.sub.status === "ended") {
      e = { key, sub: this.comet.subscribe(name, args), refs: 0 };
      this.#entries.set(key, e);
    }
    clearTimeout(e.stopTimer);
    e.stopTimer = undefined;
    e.refs++;
    return { registry: this, entry: e, released: false };
  }

  release(lease: Lease): void {
    if (lease.released) return;
    lease.released = true;
    const e = lease.entry;
    if (--e.refs > 0) return;
    e.stopTimer = setTimeout(() => {
      if (e.refs > 0 || this.#entries.get(e.key) !== e) return;
      this.#entries.delete(e.key);
      e.stopped = true;
      e.sub.stop();
    }, 0);
  }
}

/** Canonical key for (name, args): JSON with object keys sorted. */
function subKey(name: string, args: unknown): string {
  return JSON.stringify([name, args ?? null], (_k, v) =>
    v && typeof v === "object" && !Array.isArray(v)
      ? Object.fromEntries(Object.keys(v).sort().map((k) => [k, v[k]]))
      : v,
  );
}

// ---- provider ----

const CometContext = createContext<Registry | null>(null);

// One registry per Comet, so subscriptions survive anything that recreates
// the provider's state, and providers sharing a client share its subs.
const registries = new WeakMap<Comet, Registry>();

function registryFor(client: Comet): Registry {
  let r = registries.get(client);
  if (!r) registries.set(client, (r = new Registry(client)));
  return r;
}

export interface CometProviderProps {
  client: Comet | TypedComet<any>;
  children?: ReactNode;
}

/** Makes a Comet available to the hooks below. */
export function CometProvider({ client, children }: CometProviderProps): ReactNode {
  const registry = registryFor(client as Comet);
  return createElement(CometContext.Provider, { value: registry }, children);
}

function useRegistry(): Registry {
  const r = useContext(CometContext);
  if (!r) throw new Error("cometgo: hook used outside <CometProvider>");
  return r;
}

/** Returns the provider's Comet. */
export function useComet(): Comet {
  return useRegistry().comet;
}

// ---- useSubscription ----

export interface SubState<T> {
  rows: readonly T[];
  status: SubStatus;
  error?: CometError;
}

const LOADING: SubState<never> = { rows: [], status: "loading" };
const noopSubscribe = () => () => {};
const loadingSnapshot = () => LOADING;

// Cached per entry so the snapshot only changes identity when a field does.
function snapshot<T>(e: Entry): SubState<T> {
  const { rows, status, error } = e.sub;
  const s = e.snap;
  if (s && s.rows === rows && s.status === status && s.error === error) return s;
  return (e.snap = { rows, status, error });
}

/**
 * Subscribes to a publication for the lifetime of the component.
 *
 * When args change (by value), the previous rows stay on screen until the
 * new subscription is ready; only then is the old one released.
 */
export function useSubscription<T = Record<string, unknown>>(name: string, args?: unknown): SubState<T> {
  const registry = useRegistry();
  const key = subKey(name, args);
  const [shown, setShown] = useState<Lease | null>(null);
  // Every lease this component holds; at rest, exactly the shown one.
  const held = useRef(new Set<Lease>());

  useEffect(() => {
    const lease = registry.acquire(key, name, args);
    held.current.add(lease);
    let promoted = false;
    const promote = () => {
      if (promoted) return;
      promoted = true;
      for (const l of held.current) {
        if (l !== lease) {
          registry.release(l);
          held.current.delete(l);
        }
      }
      setShown(lease);
    };
    const { sub } = lease.entry;
    const settled = () => sub.status === "ready" || sub.status === "ended";
    // Show at once when there is nothing on screen worth keeping, when the
    // sub is already settled, or when it is the very sub on screen (a remount).
    if (!showable(shown, registry) || shown!.entry === lease.entry || settled()) {
      promote();
      // The shown lease is released by the next promotion or by unmount.
      return;
    }
    const unlisten = sub.subscribe(() => {
      if (settled()) promote();
    });
    return () => {
      unlisten();
      if (!promoted) {
        registry.release(lease);
        held.current.delete(lease);
      }
    };
    // `shown` is read only to decide whether to wait; re-running on it
    // would re-acquire.
  }, [registry, key]);

  useEffect(() => {
    const leases = held.current;
    return () => {
      for (const l of leases) registry.release(l);
      leases.clear();
    };
  }, [registry]);

  // What is on screen: the shown sub while it is still live, else whatever
  // this key already has in the registry (another component's ready sub, or
  // ours during a remount), else loading.
  const entry = showable(shown, registry) ? shown!.entry : registry.peek(key);
  return useSyncExternalStore(
    entry ? entry.sub.subscribe : noopSubscribe,
    entry ? () => snapshot<T>(entry) : loadingSnapshot,
    entry ? () => snapshot<T>(entry) : loadingSnapshot,
  );
}

// A shown lease is worth keeping on screen only while its sub is live and it
// belongs to the current client.
function showable(l: Lease | null, registry: Registry): boolean {
  return !!l && l.registry === registry && !l.entry.stopped;
}

// ---- useConnection / useCall ----

export interface ConnectionState {
  status: ConnectionStatus;
  user: unknown;
}

/** The connection status and authenticated user, re-rendering on change. */
export function useConnection(): ConnectionState {
  const comet = useComet();
  const cache = useRef<ConnectionState | null>(null);
  const subscribe = useCallback((fn: () => void) => comet.onChange(fn), [comet]);
  const get = useCallback(() => {
    const c = cache.current;
    if (c && c.status === comet.status && c.user === comet.user) return c;
    return (cache.current = { status: comet.status, user: comet.user });
  }, [comet]);
  return useSyncExternalStore(subscribe, get, get);
}

/** A stable function that calls a server method (see Comet.call). */
export function useCall(): Comet["call"] {
  const comet = useComet();
  return useCallback<Comet["call"]>((m, args, opts) => comet.call(m, args, opts), [comet]);
}

/** The hooks above, checked against a generated schema S. */
export interface TypedHooks<S extends Schema> {
  useComet(): TypedComet<S>;
  useSubscription<P extends PubName<S>>(name: P, ...rest: SubRest<PubArgs<S, P>>): SubState<PubRow<S, P>>;
  useCall(): TypedComet<S>["call"];
}

/**
 * Returns the hooks typed against S, for use with connect<S>():
 * `export const { useSubscription, useCall } = hooksFor<Schema>();`
 * They are the same functions as the untyped exports.
 */
export function hooksFor<S extends Schema>(): TypedHooks<S> {
  return { useComet, useSubscription, useCall } as unknown as TypedHooks<S>;
}

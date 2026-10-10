// React bindings for the CometGo client: @cometgo/client/react.

import {
  createContext,
  createElement,
  type ReactNode,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  useSyncExternalStore,
} from "react";
import type { Comet, CometError, ConnectionStatus, Subscription, SubStatus } from "./index.ts";

// ---- shared subscriptions ----

interface Entry {
  key: string;
  sub: Subscription<any>;
  refs: number;
  stopTimer?: ReturnType<typeof setTimeout>;
  snap?: SubState<any>;
}

/** One reference to a shared subscription, held by one hook instance. */
interface Lease {
  entry: Entry;
  released: boolean;
}

// Subscriptions are shared per provider and ref-counted by (name, canonical
// args). The last release stops the sub only after a macrotask, so a
// StrictMode unmount/remount (or a remount on the same key) re-acquires the
// same sub instead of sending unsub + sub.
class Registry {
  readonly comet: Comet;
  #entries = new Map<string, Entry>();

  constructor(comet: Comet) {
    this.comet = comet;
  }

  acquire(key: string, name: string, args: unknown): Lease {
    let e = this.#entries.get(key);
    if (!e) {
      e = { key, sub: this.comet.subscribe(name, args), refs: 0 };
      this.#entries.set(key, e);
    }
    clearTimeout(e.stopTimer);
    e.stopTimer = undefined;
    e.refs++;
    return { entry: e, released: false };
  }

  release(lease: Lease): void {
    if (lease.released) return;
    lease.released = true;
    const e = lease.entry;
    if (--e.refs > 0) return;
    e.stopTimer = setTimeout(() => {
      if (e.refs > 0 || this.#entries.get(e.key) !== e) return;
      this.#entries.delete(e.key);
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

export interface CometProviderProps {
  client: Comet;
  children?: ReactNode;
}

/** Makes a Comet available to the hooks below. */
export function CometProvider({ client, children }: CometProviderProps): ReactNode {
  const registry = useMemo(() => new Registry(client), [client]);
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
    // Show at once when there is nothing on screen to keep, when the sub is
    // already settled, or when it is the very sub on screen (a remount).
    if (!shown || shown.entry === lease.entry || settled()) {
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

  const entry = shown?.entry;
  return useSyncExternalStore(
    entry ? entry.sub.subscribe : noopSubscribe,
    entry ? () => snapshot<T>(entry) : loadingSnapshot,
    entry ? () => snapshot<T>(entry) : loadingSnapshot,
  );
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

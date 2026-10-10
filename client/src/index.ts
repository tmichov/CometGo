// CometGo TypeScript client (wire protocol v1, PROTOCOL.md).

import { type Op, RowSet, type SubInit, rowDecoder } from "./store.ts";

export { decoderFor, RowSet } from "./store.ts";
export type { Op, SubInit } from "./store.ts";

export const SUBPROTOCOL = "cometgo.1.json";

export class CometError extends Error {
  readonly code: string;
  readonly details?: unknown;
  readonly retryAfterMs?: number;
  constructor(e: { code: string; msg?: string; details?: unknown; retryAfterMs?: number }) {
    super(e.msg || e.code);
    this.name = "CometError";
    this.code = e.code;
    this.details = e.details;
    this.retryAfterMs = e.retryAfterMs;
  }
}

export type TokenSource = string | null | (() => string | null | Promise<string | null>);

export interface ConnectOptions {
  /** WebSocket URL. Defaults to ws(s)://<current host>/_comet. */
  url?: string;
  /** Bearer token, or a function returning one. Omit for cookie sessions. */
  token?: TokenSource;
  /** This client's build id; compared with the server's in welcome. */
  build?: string;
  /** WebSocket implementation, for non-browser environments. */
  WebSocket?: typeof WebSocket;
}

export interface CallOptions {
  /** Skip the read-your-writes fence (§6.2); for calls that do not write. */
  noFence?: boolean;
  /**
   * Attach a generated idempotency key (§5), so the call is re-sent after a
   * disconnect instead of failing with CONNECTION_LOST. The server runs the
   * method at most once per key and user, and returns the stored result to
   * a retry. Use it for calls that must not run twice.
   */
  idempotent?: boolean;
  /** An idempotency key of your own (implies idempotent), for retries across page loads. */
  ik?: string;
}

/**
 * How long after its first send an ik call may still be retried. Must stay
 * well under the server's IdempotencyTTL (24h by default): past the TTL the
 * stored result is gone and a retry would run the method again.
 */
const IK_RETRY_WINDOW_MS = 60 * 60 * 1000;

const lost = () => new CometError({ code: "CONNECTION_LOST", msg: "connection lost; outcome unknown" });

export type ConnectionStatus = "connecting" | "connected" | "disconnected";
export type SubStatus = "loading" | "ready" | "resyncing" | "ended";

interface WireEntry {
  s: number;
  init?: SubInit;
  reset?: boolean;
  ops?: Op[];
  ready?: boolean;
  end?: { code: string; msg?: string; details?: unknown };
}

interface PendingCall {
  id: number;
  m: string;
  args: unknown;
  noFence: boolean;
  ik?: string;
  sent: boolean;
  /** When the call was first sent; bounds how long an ik call is retried. */
  firstSent?: number;
  /**
   * Set when an ik call lost its connection after being sent. It is held
   * until the next welcome, which decides whether it is re-sent: the server
   * must advertise the ik cap and the user must be the one it ran as.
   */
  retry?: { user: string };
  resolve: (r: { value: unknown; lag: boolean }) => void;
  reject: (e: CometError) => void;
}

export function connect(opts: ConnectOptions = {}): Comet {
  return new Comet(opts);
}

export class Comet {
  status: ConnectionStatus = "connecting";
  /** The authenticated user as sent by the server, or null when anonymous. */
  user: unknown = null;
  /** Server's client build id from the last welcome. */
  serverBuild?: string;

  #opts: ConnectOptions;
  #ws?: WebSocket;
  #subs = new Map<number, Subscription<any>>();
  #calls = new Map<number, PendingCall>();
  #auths = new Map<number, { resolve: (u: unknown) => void; reject: (e: CometError) => void }>();
  #nextId = 1;
  #nextSub = 1;
  #txParts: WireEntry[] = [];
  #attempt = 0;
  #hb = 25_000;
  #lastRecv = 0;
  #pinged = false;
  #closed = false;
  /** Whether the current connection has received its welcome. */
  #welcomed = false;
  #reconnectTimer?: ReturnType<typeof setTimeout>;
  #hbTimer?: ReturnType<typeof setInterval>;
  #statusListeners = new Set<() => void>();

  constructor(opts: ConnectOptions) {
    this.#opts = opts;
    if (typeof window !== "undefined") {
      window.addEventListener("online", () => this.#reconnectNow());
      document.addEventListener("visibilitychange", () => {
        if (document.visibilityState === "visible") this.#reconnectNow();
      });
    }
    this.#hbTimer = setInterval(() => this.#checkLiveness(), 1000);
    void this.#open();
  }

  /** Listen for status/user changes. Returns an unsubscribe function. */
  onChange(fn: () => void): () => void {
    this.#statusListeners.add(fn);
    return () => this.#statusListeners.delete(fn);
  }

  subscribe<T = Record<string, unknown>>(name: string, args?: unknown): Subscription<T> {
    const sub = new Subscription<T>(this, this.#nextSub++, name, args);
    this.#subs.set(sub.id, sub);
    if (this.#isOpen()) this.#sendSub(sub);
    return sub;
  }

  /** Calls a server method. Resolves after its effects are in the store (§6.2). */
  async call<R = unknown>(m: string, args?: unknown, opts: CallOptions = {}): Promise<R> {
    return (await this.callWithStatus<R>(m, args, opts)).value;
  }

  /** Like call, but also reports lag: true when the fence timed out. */
  callWithStatus<R = unknown>(m: string, args?: unknown, opts: CallOptions = {}): Promise<{ value: R; lag: boolean }> {
    return new Promise((resolve, reject) => {
      const c: PendingCall = {
        id: this.#nextId++,
        m,
        args,
        noFence: !!opts.noFence,
        ik: opts.ik ?? (opts.idempotent ? uuid() : undefined),
        sent: false,
        resolve: resolve as PendingCall["resolve"],
        reject,
      };
      this.#calls.set(c.id, c);
      // Unsent calls wait for the next connection; they never ran, so
      // sending them later is safe.
      if (this.#isOpen()) this.#sendCall(c);
    });
  }

  /** Replaces the token (login, refresh) or logs out with null (§3.2). */
  setToken(token: TokenSource): Promise<unknown> {
    this.#opts.token = token;
    if (!this.#isOpen()) return Promise.resolve(this.user);
    return (async () => {
      const t = await resolveToken(token);
      const id = this.#nextId++;
      return new Promise((resolve, reject) => {
        this.#auths.set(id, { resolve, reject });
        this.#send({ t: "auth", id, token: t });
      });
    })();
  }

  close(): void {
    this.#closed = true;
    // Calls held for a retry would otherwise never settle.
    for (const c of [...this.#calls.values()]) {
      if (c.retry) {
        this.#calls.delete(c.id);
        c.reject(lost());
      }
    }
    clearTimeout(this.#reconnectTimer);
    clearInterval(this.#hbTimer);
    this.#ws?.close(1000);
  }

  // ---- internals used by Subscription ----

  _unsubscribe(sub: Subscription<any>): void {
    if (this.#subs.delete(sub.id) && this.#isOpen()) this.#send({ t: "unsub", s: sub.id });
  }

  // ---- connection ----

  #isOpen(): boolean {
    return this.#ws?.readyState === 1;
  }

  async #open(): Promise<void> {
    if (this.#closed) return;
    this.#setStatus("connecting");
    let token: string | null = null;
    try {
      token = await resolveToken(this.#opts.token);
    } catch (e) {
      console.warn("cometgo: token source failed", e);
    }
    const WS = this.#opts.WebSocket ?? WebSocket;
    const ws = new WS(this.#opts.url ?? defaultUrl(), [SUBPROTOCOL]);
    this.#ws = ws;
    ws.onopen = () => {
      // Pipelined handshake (§3.1): no need to wait for welcome.
      this.#lastRecv = Date.now();
      this.#send({ t: "hello", v: 1, caps: ["ik"], ...(token ? { auth: { token } } : {}), ...(this.#opts.build ? { build: this.#opts.build } : {}) });
      for (const sub of this.#subs.values()) this.#sendSub(sub);
      // Held ik retries wait for welcome (see PendingCall.retry).
      for (const c of this.#calls.values()) if (!c.sent && !c.retry) this.#sendCall(c);
    };
    ws.onmessage = (ev) => {
      this.#lastRecv = Date.now();
      this.#pinged = false;
      let msg: any;
      try {
        msg = JSON.parse(ev.data as string);
      } catch {
        return;
      }
      this.#handle(msg);
    };
    ws.onclose = (ev) => {
      if (this.#ws !== ws) return;
      this.#onDisconnect(ev.code, ev.reason);
    };
  }

  #onDisconnect(code: number, reason: string): void {
    this.#ws = undefined;
    this.#txParts = [];
    // Calls that reached the server have an unknown outcome: never replay them
    // (§7.1), unless they carry an idempotency key. Those are re-sent on the
    // next connection, and the server answers with the stored result if the
    // first attempt committed. Without this connection's welcome we do not
    // know which user the call ran as, so it cannot be retried safely. After
    // close() there is no next connection.
    const welcomed = this.#welcomed;
    this.#welcomed = false;
    for (const c of [...this.#calls.values()]) {
      if (!c.sent) continue;
      c.sent = false;
      if (c.ik !== undefined && welcomed && !this.#closed) {
        c.retry = { user: JSON.stringify(this.user) };
      } else {
        this.#calls.delete(c.id);
        c.reject(lost());
      }
    }
    for (const a of this.#auths.values()) a.reject(new CometError({ code: "CONNECTION_LOST" }));
    this.#auths.clear();
    for (const sub of this.#subs.values()) sub._disconnected();
    this.#setStatus("disconnected");
    if (code >= 4400 && code < 4500 && reason) console.warn("cometgo: closed", code, reason);
    if (this.#closed) return;
    // Exponential backoff with full jitter.
    const delay = Math.random() * Math.min(30_000, 500 * 2 ** this.#attempt++);
    this.#reconnectTimer = setTimeout(() => void this.#open(), delay);
  }

  #reconnectNow(): void {
    if (this.#closed || this.#ws) return;
    clearTimeout(this.#reconnectTimer);
    void this.#open();
  }

  #checkLiveness(): void {
    if (!this.#isOpen()) return;
    const idle = Date.now() - this.#lastRecv;
    if (idle > this.#hb + 10_000) {
      this.#ws!.close(4408); // dead connection; onclose reconnects
    } else if (idle > this.#hb && !this.#pinged) {
      this.#pinged = true;
      this.#send({ t: "ping" });
    }
  }

  #send(msg: unknown): void {
    this.#ws?.send(JSON.stringify(msg));
  }

  #sendSub(sub: Subscription<any>): void {
    this.#send({ t: "sub", s: sub.id, name: sub.name, args: sub.args ?? null, ...(sub._hasData() ? { have: true } : {}) });
  }

  #sendCall(c: PendingCall): void {
    c.sent = true;
    c.firstSent ??= Date.now();
    this.#send({ t: "call", id: c.id, m: c.m, args: c.args ?? null, ...(c.noFence ? { noFence: true } : {}), ...(c.ik !== undefined ? { ik: c.ik } : {}) });
  }

  // Re-sends ik calls held since a disconnect, or rejects those it cannot
  // retry safely: a server without the ik cap ignores the key and would run
  // the call again, as would a different user's scope or an expired result.
  #retryHeld(serverHasIK: boolean): void {
    const user = JSON.stringify(this.user);
    for (const c of [...this.#calls.values()]) {
      if (!c.retry) continue;
      if (serverHasIK && c.retry.user === user && Date.now() - c.firstSent! < IK_RETRY_WINDOW_MS) {
        c.retry = undefined;
        this.#sendCall(c);
      } else {
        this.#calls.delete(c.id);
        c.reject(lost());
      }
    }
  }

  #setStatus(s: ConnectionStatus): void {
    if (this.status === s) return;
    this.status = s;
    this.#emit();
  }

  #emit(): void {
    for (const fn of this.#statusListeners) fn();
  }

  #handle(msg: any): void {
    switch (msg.t) {
      case "welcome":
        this.#attempt = 0;
        this.#hb = msg.hb ?? this.#hb;
        this.user = msg.user ?? null;
        this.serverBuild = msg.build;
        this.#welcomed = true;
        this.#retryHeld(Array.isArray(msg.caps) && msg.caps.includes("ik"));
        this.status = "connected";
        this.#emit();
        return;
      case "tx": {
        this.#txParts.push(...(msg.subs as WireEntry[]));
        if (msg.more) return; // wait for the final part (§4.3)
        const entries = this.#txParts;
        this.#txParts = [];
        this.#applyTx(entries);
        return;
      }
      case "res": {
        const c = this.#calls.get(msg.id);
        if (!c) return;
        this.#calls.delete(msg.id);
        if (msg.ok) c.resolve({ value: msg.val, lag: !!msg.lag });
        else c.reject(new CometError(msg.err));
        return;
      }
      case "authRes": {
        if (msg.ok || msg.id === undefined) this.user = msg.ok ? (msg.user ?? null) : null;
        if (msg.id !== undefined) {
          const a = this.#auths.get(msg.id);
          this.#auths.delete(msg.id);
          if (msg.ok) a?.resolve(this.user);
          else {
            this.user = null;
            a?.reject(new CometError(msg.err));
          }
        }
        this.#emit();
        return;
      }
    }
  }

  // One tx is applied as one store transaction, then each touched sub
  // notifies its listeners once (§4.2).
  #applyTx(entries: WireEntry[]): void {
    const touched = new Set<Subscription<any>>();
    for (const e of entries) {
      const sub = this.#subs.get(e.s);
      if (!sub) continue; // late data for an unsubscribed id
      try {
        sub._entry(e);
      } catch (err) {
        console.error("cometgo: could not apply ops, resyncing", sub.name, err);
        this.#resync(sub);
        continue;
      }
      if (e.end) this.#subs.delete(sub.id);
      touched.add(sub);
    }
    for (const sub of touched) sub._flush();
  }

  // Re-requests a full snapshot for a sub whose local state diverged.
  #resync(sub: Subscription<any>): void {
    this.#subs.delete(sub.id);
    if (this.#isOpen()) this.#send({ t: "unsub", s: sub.id });
    sub._rekey(this.#nextSub++);
    this.#subs.set(sub.id, sub);
    if (this.#isOpen()) this.#sendSub(sub);
  }
}

export class Subscription<T> {
  /** Current rows, in server order. Replaced (never mutated) on change. */
  rows: readonly T[] = [];
  status: SubStatus = "loading";
  error?: CometError;
  /** Resolves when the first snapshot is complete. */
  readonly ready: Promise<void>;

  #comet: Comet;
  #id: number;
  #set = new RowSet<T>();
  #pending?: RowSet<T>;
  #init?: SubInit;
  #decode?: (cells: unknown[]) => T;
  #dirty = false;
  #listeners = new Set<() => void>();
  #resolveReady!: () => void;
  #rejectReady!: (e: CometError) => void;

  readonly name: string;
  readonly args: unknown;

  constructor(comet: Comet, id: number, name: string, args: unknown) {
    this.#comet = comet;
    this.#id = id;
    this.name = name;
    this.args = args;
    this.ready = new Promise((res, rej) => {
      this.#resolveReady = res;
      this.#rejectReady = rej;
    });
    this.ready.catch(() => {}); // an ended sub need not have a ready awaiter
  }

  get id(): number {
    return this.#id;
  }

  /** Listen for row/status changes. Compatible with React's useSyncExternalStore. */
  subscribe = (fn: () => void): (() => void) => {
    this.#listeners.add(fn);
    return () => this.#listeners.delete(fn);
  };

  getSnapshot = (): readonly T[] => this.rows;

  stop(): void {
    this.#comet._unsubscribe(this);
    this.status = "ended";
  }

  _hasData(): boolean {
    return this.#init !== undefined;
  }

  _rekey(id: number): void {
    this.#id = id;
    this.#pending = undefined;
  }

  _disconnected(): void {
    this.#pending = undefined;
    if (this.status === "ready") {
      this.status = "resyncing";
      this.#dirty = true;
      this._flush();
    }
  }

  _entry(e: WireEntry): void {
    if (e.init) {
      const shapeChanged = this.#init !== undefined && JSON.stringify(this.#init) !== JSON.stringify(e.init);
      this.#init = e.init;
      this.#decode = rowDecoder<T>(e.init);
      if (shapeChanged) this.#set = new RowSet<T>(); // cached objects used the old columns
    }
    // A reset (or a fresh snapshot over data we hold) is buffered and swapped
    // in at ready, so stale rows stay visible meanwhile (§4.4).
    if (e.reset || (e.init && this.#set.keys.length > 0)) this.#pending = new RowSet<T>();
    const target = this.#pending ?? this.#set;
    for (const op of e.ops ?? []) target.apply(op);
    if (e.ready) {
      if (this.#pending) {
        this.#set.adopt(this.#pending);
        this.#pending = undefined;
      }
      this.status = "ready";
      this.#resolveReady();
    }
    if (e.end) {
      this.#set = new RowSet<T>();
      this.#pending = undefined;
      this.status = "ended";
      if (e.end.code) {
        this.error = new CometError(e.end);
        this.#rejectReady(this.error);
      }
    }
    this.#dirty = true;
  }

  _flush(): void {
    if (!this.#dirty) return;
    this.#dirty = false;
    if (this.#decode) this.rows = this.#set.materialize(this.#decode);
    else this.rows = [];
    for (const fn of this.#listeners) fn();
  }
}

async function resolveToken(t: TokenSource | undefined): Promise<string | null> {
  if (typeof t === "function") return (await t()) ?? null;
  return t ?? null;
}

// A random (v4) UUID. crypto.randomUUID needs a secure context; getRandomValues does not.
function uuid(): string {
  const b = crypto.getRandomValues(new Uint8Array(16));
  b[6] = (b[6]! & 0x0f) | 0x40;
  b[8] = (b[8]! & 0x3f) | 0x80;
  const h = [...b].map((x) => x.toString(16).padStart(2, "0")).join("");
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

function defaultUrl(): string {
  const { protocol, host } = globalThis.location;
  return `${protocol === "https:" ? "wss" : "ws"}://${host}/_comet`;
}

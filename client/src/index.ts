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
  /** WebSocket URL; a relative or http(s) one becomes ws(s) on the page's
   * host. Defaults to ws(s)://<current host>/_comet. */
  url?: string;
  /** Bearer token, or a function returning one. Omit for cookie sessions. */
  token?: TokenSource;
  /** This client's build id; compared with the server's in welcome. */
  build?: string;
  /** WebSocket implementation, for non-browser environments. */
  WebSocket?: typeof WebSocket;
  /** Sent as the `route` query parameter on every connect, for a load
   * balancer that hashes on it to keep reconnects (and a tenant's users) on
   * one node. Use an opaque id, e.g. the tenant id: it appears in access
   * logs. Defaults to a random id kept for this client's lifetime. The
   * server ignores it. */
  routeKey?: string;
}

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

// The source a setToken stored, and the one it replaced (restored if the
// server refuses the new one).
interface TokenChange {
  source: TokenSource;
  prev: TokenSource | undefined;
}

interface PendingAuth {
  token: string | null;
  change: TokenChange;
  // Sent on a socket that dropped; awaiting the resume outcome.
  held?: boolean;
  resolve: (u: unknown) => void;
  reject: (e: CometError) => void;
}

// Ack the last seq received at least this often (§7.2): every liveness
// check (1 s) or after this many bytes.
const ACK_BYTES = 64 << 10;

export interface CallOptions {
  /** Skip the read-your-writes fence (§6.2), for writes that need not wait.
   * A call whose method writes nothing skips it anyway. */
  noFence?: boolean;
  /**
   * Cancels the call when aborted (§5). A call not yet sent is rejected at
   * once with CANCELED. A sent one is asked to stop, and the promise settles
   * from the server's answer: CANCELED if the method had not committed yet,
   * otherwise its real result.
   */
  signal?: AbortSignal;
  /**
   * Attach a generated idempotency key (§5), so the call can be re-sent
   * after a disconnect even when the session cannot be resumed, instead of
   * failing with CONNECTION_LOST. The server runs the method at most once
   * per key and user, and returns the stored result to a retry. Use it for
   * calls that must not run twice.
   */
  idempotent?: boolean;
  /**
   * An idempotency key of your own (implies idempotent), for retries across
   * page loads. It must be unguessable, e.g. crypto.randomUUID(): anonymous
   * callers share one key scope.
   */
  ik?: string;
}

/**
 * How long after its first send an ik call may still be retried on a new
 * session. Must stay well under the server's IdempotencyTTL (24h by
 * default): past the TTL the stored result is gone and a retry would run
 * the method again.
 */
const IK_RETRY_WINDOW_MS = 60 * 60 * 1000;

interface PendingCall {
  id: number;
  m: string;
  args: unknown;
  noFence: boolean;
  ik?: string;
  sent: boolean;
  /** When the call was first sent; bounds how long an ik call is retried. */
  firstSent?: number;
  // Sent on a socket that dropped; awaiting the next welcome, which re-sends
  // it if the session resumed (§7.2) or, for an ik call, if it can be
  // retried safely on the new session (§7.1).
  held?: boolean;
  // For a held ik call: the user it ran as, if the dropped socket's welcome
  // had arrived. Without it the call cannot be retried on a new session.
  heldUser?: string;
  // The caller aborted: send cancel after the call whenever it is sent.
  cancel?: boolean;
  resolve: (r: { value: unknown; lag: boolean }) => void;
  reject: (e: CometError) => void;
}

/**
 * Opens a connection. With a Schema type argument (as generated on the
 * server by cometgo.Describe), method names, args, results, publication
 * names and rows are checked: `connect<Schema>()`.
 */
export function connect(opts?: ConnectOptions): Comet;
export function connect<S extends Schema>(opts?: ConnectOptions): TypedComet<S>;
export function connect(opts: ConnectOptions = {}): Comet {
  return new Comet(opts);
}

/**
 * The shape of a generated schema: per method its args and result, per
 * publication its args and row.
 */
export interface Schema {
  methods: object;
  publications: object;
}

export type MethodName<S extends Schema> = keyof S["methods"] & string;
export type MethodArgs<S extends Schema, M extends MethodName<S>> = S["methods"][M] extends { args: infer A } ? A : never;
export type MethodResult<S extends Schema, M extends MethodName<S>> = S["methods"][M] extends { result: infer R } ? R : never;
export type PubName<S extends Schema> = keyof S["publications"] & string;
export type PubArgs<S extends Schema, P extends PubName<S>> = S["publications"][P] extends { args: infer A } ? A : never;
export type PubRow<S extends Schema, P extends PubName<S>> = S["publications"][P] extends { row: infer R } ? R : never;

/** Args may be left out when they accept null (Go args of struct{}). */
export type CallRest<A> = null extends A ? [args?: A, opts?: CallOptions] : [args: A, opts?: CallOptions];
export type SubRest<A> = null extends A ? [args?: A] : [args: A];

/** A Comet whose calls and subscriptions are checked against S. */
export interface TypedComet<S extends Schema> extends Omit<Comet, "call" | "callWithStatus" | "subscribe"> {
  call<M extends MethodName<S>>(m: M, ...rest: CallRest<MethodArgs<S, M>>): Promise<MethodResult<S, M>>;
  callWithStatus<M extends MethodName<S>>(m: M, ...rest: CallRest<MethodArgs<S, M>>): Promise<{ value: MethodResult<S, M>; lag: boolean }>;
  subscribe<P extends PubName<S>>(name: P, ...rest: SubRest<PubArgs<S, P>>): Subscription<PubRow<S, P>>;
}

export class Comet {
  status: ConnectionStatus = "connecting";
  /** The authenticated user as sent by the server, or null when anonymous. */
  user: unknown = null;
  /** Server's client build id from the last welcome. */
  serverBuild?: string;

  #opts: ConnectOptions;
  // Random by default so keyless clients do not all hash to one node.
  #url: string;
  #ws?: WebSocket;
  #subs = new Map<number, Subscription<any>>();
  #calls = new Map<number, PendingCall>();
  #auths = new Map<number, PendingAuth>();
  // Every unsettled setToken's change, from the call on: a refusal must
  // reach one whose token function is still resolving, not yet in #auths.
  #tokenChanges = new Set<TokenChange>();
  #nextId = 1;
  #nextSub = 1;
  #txParts: WireEntry[] = [];
  #attempt = 0;
  #hb = 25_000;
  #lastRecv = 0;
  #pinged = false;
  #closed = false;
  #reconnectTimer?: ReturnType<typeof setTimeout>;
  // Proactive refresh of a function token source before its authExp (§3.2).
  #refreshTimer?: ReturnType<typeof setTimeout>;
  #authExp?: number; // the current identity's expiry, Unix seconds
  #hbTimer?: ReturnType<typeof setInterval>;
  #statusListeners = new Set<() => void>();
  // Session resume (§7.2): the session to resume, whether the server offers
  // resume, the last seq fully received, the last seq acked, and bytes
  // received since.
  #session?: string;
  #canResume = false;
  #seq = 0;
  #acked = 0;
  #welcomed = false; // the current socket's welcome has arrived
  #helloSent = false; // the current socket's hello has gone out
  #unackedBytes = 0;
  // retryInMs of a goaway on the current socket (§7.3): the server is
  // shutting down, so new calls wait for the next connection, which is
  // opened after this delay instead of the usual backoff.
  #goaway?: number;
  // Kept so close() can remove them. Not an AbortController signal: jsdom
  // rejects a signal from another realm, such as Node's global one.
  #onOnline = () => this.#reconnectNow();
  #onVisible = () => {
    if (document.visibilityState === "visible") this.#reconnectNow();
  };

  constructor(opts: ConnectOptions) {
    this.#opts = opts;
    // Built here so a bad url throws from connect(), not inside #open.
    this.#url = routeUrl(opts.url, opts.routeKey ?? uuid());
    if (typeof window !== "undefined") {
      window.addEventListener("online", this.#onOnline);
      document.addEventListener("visibilitychange", this.#onVisible);
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
    if (this.#closed) {
      endClosed(sub);
      return sub;
    }
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
      const { signal } = opts;
      if (signal?.aborted || this.#closed) {
        reject(canceled());
        return;
      }
      const onAbort = () => this.#abort(c);
      const settled = () => signal?.removeEventListener("abort", onAbort);
      const c: PendingCall = {
        id: this.#nextId++,
        m,
        args,
        noFence: !!opts.noFence,
        ik: opts.ik ?? (opts.idempotent ? uuid() : undefined),
        sent: false,
        resolve: (r) => {
          settled();
          resolve(r as { value: R; lag: boolean });
        },
        reject: (e) => {
          settled();
          reject(e);
        },
      };
      signal?.addEventListener("abort", onAbort, { once: true });
      this.#calls.set(c.id, c);
      // Unsent calls wait for the next connection; they never ran, so
      // sending them later is safe.
      if (this.#isOpen() && this.#goaway === undefined) this.#sendCall(c);
    });
  }

  /** Replaces the token (login, refresh) or logs out with null (§3.2). */
  setToken(token: TokenSource): Promise<unknown> {
    const change: TokenChange = { source: token, prev: this.#opts.token };
    this.#opts.token = token;
    // The next hello sends the stored token, even if its socket is already
    // opening (see #open).
    if (!this.#isOpen()) return Promise.resolve(this.user);
    this.#tokenChanges.add(change);
    return (async () => {
      const t = await resolveToken(token);
      // The socket dropped meanwhile: the stored token goes in the next hello.
      if (!this.#isOpen()) throw lost();
      const id = this.#nextId++;
      return new Promise((resolve, reject) => {
        this.#auths.set(id, { token: t, change, resolve, reject });
        this.#send({ t: "auth", id, token: t });
      });
    })().finally(() => this.#tokenChanges.delete(change));
  }

  close(): void {
    this.#closed = true;
    if (typeof window !== "undefined") {
      window.removeEventListener("online", this.#onOnline);
      document.removeEventListener("visibilitychange", this.#onVisible);
    }
    clearTimeout(this.#reconnectTimer);
    clearTimeout(this.#refreshTimer);
    clearInterval(this.#hbTimer);
    this.#ws?.close(1000);
    this.#rejectSent(); // calls kept for a resume that will not happen
    // The rest were never sent, so never ran.
    for (const c of this.#calls.values()) c.reject(canceled());
    this.#calls.clear();
    // A sub still loading would never get its first snapshot, so its ready
    // would never settle. Subs with data keep it on screen.
    for (const sub of this.#subs.values()) if (sub.status === "loading") endClosed(sub);
  }

  // ---- internals used by Subscription ----

  _unsubscribe(sub: Subscription<any>): void {
    if (this.#subs.delete(sub.id) && this.#isOpen()) this.#send({ t: "unsub", s: sub.id });
  }

  // ---- connection ----

  // Open and past hello: only then may anything else be sent.
  #isOpen(): boolean {
    return this.#ws?.readyState === 1 && this.#helloSent;
  }

  async #open(): Promise<void> {
    if (this.#closed) return;
    this.#setStatus("connecting");
    const first = await this.#credential();
    if (!first) {
      // Connecting anonymously would end a resumable session and look like a
      // logout (§7.2), so retry with backoff instead.
      if (this.#closed || this.#ws) return;
      this.#setStatus("disconnected");
      this.#scheduleReconnect();
      return;
    }
    // While the token resolved, close() may have run, or #reconnectNow may
    // have started another #open that already has its socket.
    if (this.#closed || this.#ws) return;
    const WS = this.#opts.WebSocket ?? WebSocket;
    let cred = first;
    const ws = new WS(this.#url, [SUBPROTOCOL]);
    this.#ws = ws;
    this.#helloSent = false;
    ws.onopen = async () => {
      // Pipelined handshake (§3.1): no need to wait for welcome. Calls that
      // were sent before the drop wait for it, though: they are re-sent only
      // if the session resumes (§7.2).
      this.#lastRecv = Date.now();
      this.#welcomed = false;
      this.#goaway = undefined;
      // A setToken while the socket was opening: hello carries its token.
      while (cred.source !== this.#opts.token) {
        const next = await this.#credential();
        if (this.#ws !== ws || ws.readyState !== 1) return;
        if (!next) {
          // As in #open: no anonymous hello. Drop this socket before hello
          // (no session is touched) and reconnect with backoff.
          ws.close(4000);
          this.#onDisconnect(4000, "token source failed"); // its onclose then sees another #ws
          return;
        }
        cred = next;
      }
      const { token } = cred;
      const resume = this.#resumable() ? { resume: { session: this.#session, seq: this.#seq, subs: [...this.#subs.keys()] } } : {};
      this.#send({ t: "hello", v: 1, caps: ["resume", "evt", "ik"], ...(token ? { auth: { token } } : {}), ...(this.#opts.build ? { build: this.#opts.build } : {}), ...resume });
      this.#helloSent = true;
      for (const sub of this.#subs.values()) this.#sendSub(sub);
      for (const c of this.#calls.values()) if (!c.sent) this.#sendCall(c);
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
      if (msg.seq !== undefined) this.#unackedBytes += (ev.data as string).length;
      this.#handle(msg);
      if (this.#unackedBytes >= ACK_BYTES) this.#ack();
    };
    ws.onclose = (ev) => {
      if (this.#ws !== ws) return;
      this.#onDisconnect(ev.code, ev.reason);
    };
  }

  // The stored token source and its token, or undefined (logged) if the
  // source threw.
  async #credential(): Promise<{ source: TokenSource | undefined; token: string | null } | undefined> {
    const source = this.#opts.token;
    try {
      return { source, token: await resolveToken(source) };
    } catch (e) {
      console.warn("cometgo: token source failed", e);
      return undefined;
    }
  }

  // Calls setToken again with a function token source at about 80% of the
  // identity's remaining lifetime, so it never reaches AUTH_EXPIRED (§3.2).
  // shortcut: the 5 s floor stops a refresh loop when the client clock runs
  // ahead of authExp, but then refreshes every 5 s; measure skew if that bites.
  #scheduleRefresh(authExp: unknown): void {
    clearTimeout(this.#refreshTimer);
    this.#authExp = typeof authExp === "number" ? authExp : undefined;
    const source = this.#opts.token;
    if (typeof authExp !== "number" || typeof source !== "function") return;
    const delay = Math.min(2 ** 31 - 1, Math.max(5_000, 0.8 * (authExp * 1000 - Date.now())));
    this.#refreshTimer = setTimeout(() => {
      if (this.#opts.token === source && this.#isOpen()) this.setToken(source).catch(() => {});
    }, delay);
  }

  #onDisconnect(code: number, reason: string): void {
    this.#ws = undefined;
    clearTimeout(this.#refreshTimer); // the next welcome reschedules it
    this.#txParts = []; // a partial tx is replayed whole: #seq never counted it
    // With a resumable session, sent calls are held until the reconnect:
    // resumed, they are re-sent; otherwise they are rejected then (§7.2).
    // Calls with an idempotency key are held either way, and may be retried
    // on a new session (§5).
    const user = this.#welcomed ? JSON.stringify(this.user) : undefined;
    const resumable = !this.#closed && this.#resumable();
    for (const c of [...this.#calls.values()]) {
      if (!c.sent) continue;
      // Held from an earlier drop and not re-sent since: keep its user.
      if (c.held && c.ik !== undefined && !this.#closed) continue;
      if (resumable || (c.ik !== undefined && !this.#closed && user !== undefined)) {
        c.held = true;
        if (c.ik !== undefined) c.heldUser = user;
      } else {
        this.#calls.delete(c.id);
        c.reject(lost());
      }
    }
    for (const [id, a] of [...this.#auths]) {
      if (resumable) a.held = true;
      else {
        this.#auths.delete(id);
        a.reject(new CometError({ code: "CONNECTION_LOST" }));
      }
    }
    for (const sub of this.#subs.values()) sub._disconnected();
    this.#setStatus("disconnected");
    if (code >= 4400 && code < 4500 && reason) console.warn("cometgo: closed", code, reason);
    if (this.#closed) return;
    this.#scheduleReconnect();
  }

  // After a goaway, the server's delay spreads reconnects out; otherwise
  // exponential backoff with full jitter.
  #scheduleReconnect(): void {
    const delay = this.#goaway ?? Math.random() * Math.min(30_000, 500 * 2 ** this.#attempt++);
    this.#goaway = undefined;
    clearTimeout(this.#reconnectTimer); // two #opens can fail at once (#reconnectNow)
    this.#reconnectTimer = setTimeout(() => void this.#open(), delay);
  }

  #reconnectNow(): void {
    if (this.#closed || this.#ws) return;
    clearTimeout(this.#reconnectTimer);
    void this.#open();
  }

  #resumable(): boolean {
    return this.#canResume && this.#session !== undefined;
  }

  // Calls (and auths) that reached the server have an unknown outcome:
  // never replay them on a new session (§7.1). heldOnly limits this to those
  // sent before the last drop, sparing any sent on the current socket.
  #rejectSent(heldOnly = false): void {
    for (const c of [...this.#calls.values()]) {
      if (c.sent && (c.held || !heldOnly)) {
        this.#calls.delete(c.id);
        c.reject(lost());
      }
    }
    for (const [id, a] of [...this.#auths]) {
      if (a.held || !heldOnly) {
        this.#auths.delete(id);
        a.reject(new CometError({ code: "CONNECTION_LOST" }));
      }
    }
  }

  // On a new session, re-sends held ik calls that can be retried safely: a
  // server without the ik cap ignores the key and would run the call again,
  // as would a different user's key scope or an expired stored result.
  // The rest stay held, and #rejectSent rejects them.
  #retryIK(serverHasIK: boolean): void {
    const user = JSON.stringify(this.user);
    for (const c of this.#calls.values()) {
      if (!c.held || c.ik === undefined) continue;
      if (serverHasIK && c.heldUser === user && Date.now() - c.firstSent! < IK_RETRY_WINDOW_MS) {
        c.held = false;
        c.heldUser = undefined;
        this.#sendCall(c);
      }
    }
  }

  #ack(): void {
    this.#unackedBytes = 0;
    // Before welcome, #seq may belong to a session this socket did not resume.
    if (!this.#canResume || !this.#welcomed || this.#seq <= this.#acked || !this.#isOpen()) return;
    this.#acked = this.#seq;
    this.#send({ t: "ack", seq: this.#seq });
  }

  #checkLiveness(): void {
    if (!this.#isOpen()) return;
    this.#ack();
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
    // An aborted call held for a resume is re-sent (its outcome is still
    // unknown) and cancelled right after.
    if (c.cancel) this.#send({ t: "cancel", id: c.id });
  }

  // An unsent call never ran, so it settles here. A sent one may already
  // have committed: ask the server to cancel and wait for its res.
  #abort(c: PendingCall): void {
    if (this.#calls.get(c.id) !== c) return; // already settled
    if (!c.sent) {
      this.#calls.delete(c.id);
      c.reject(canceled());
      return;
    }
    c.cancel = true;
    if (!c.held && this.#isOpen()) this.#send({ t: "cancel", id: c.id });
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
        this.#session = msg.session;
        this.#welcomed = true;
        this.#canResume = Array.isArray(msg.caps) && msg.caps.includes("resume");
        this.#scheduleRefresh(msg.authExp);
        if (msg.resumed) {
          // Missed messages follow as a replay. Unanswered calls and auths
          // go out again with their original ids; the server runs each
          // only once (§7.2).
          for (const c of this.#calls.values()) {
            if (c.held) {
              c.held = false;
              this.#sendCall(c);
            }
          }
          for (const [id, a] of this.#auths) {
            if (a.held) {
              a.held = false;
              this.#send({ t: "auth", id, token: a.token });
            }
          }
          for (const sub of this.#subs.values()) sub._resumed();
        } else {
          this.#retryIK(Array.isArray(msg.caps) && msg.caps.includes("ik"));
          this.#rejectSent(true);
          this.#seq = this.#acked = this.#unackedBytes = 0; // a new session counts from 1
        }
        this.status = "connected";
        this.#emit();
        return;
      case "tx": {
        this.#txParts.push(...(msg.subs as WireEntry[]));
        if (msg.more) return; // wait for the final part (§4.3)
        this.#seq = msg.seq;
        const entries = this.#txParts;
        this.#txParts = [];
        this.#applyTx(entries);
        return;
      }
      case "res": {
        this.#seq = msg.seq;
        const c = this.#calls.get(msg.id);
        if (!c) return;
        this.#calls.delete(msg.id);
        if (msg.ok) c.resolve({ value: msg.val, lag: !!msg.lag });
        else c.reject(new CometError(msg.err));
        return;
      }
      case "goaway":
        // Calls already sent still settle from their res, which the server
        // sends before it closes the socket.
        this.#goaway = Math.max(0, Number(msg.retryInMs) || 0);
        return;
      case "evt": {
        this.#seq = msg.seq;
        // Events for unknown (unsubscribed, rekeyed) or ended subs are dropped.
        const sub = this.#subs.get(msg.s);
        if (sub && sub.status !== "ended") sub._event(msg.topic, msg.data);
        return;
      }
      case "authRes": {
        this.#seq = msg.seq;
        // A failed setToken keeps the current user; only an unsolicited
        // failure (expiry) downgrades to anonymous (§3.2).
        if (msg.ok) this.user = msg.user ?? null;
        else if (msg.id === undefined) this.user = null;
        if (msg.id !== undefined) {
          const a = this.#auths.get(msg.id);
          this.#auths.delete(msg.id);
          if (msg.ok) a?.resolve(this.user);
          else if (a) {
            // The server kept the current user, so the next hello must not
            // send the refused token, unless a newer setToken replaced it.
            // A newer one still pending must not fall back to it either.
            const { source, prev } = a.change;
            this.#tokenChanges.delete(a.change);
            if (this.#opts.token === source) this.#opts.token = prev;
            for (const c of this.#tokenChanges) if (c.prev === source) c.prev = prev;
            a.reject(new CometError(msg.err));
          }
        }
        // After the restore above: a refused setToken keeps the current
        // identity and its expiry, and a refused refresh is retried.
        this.#scheduleRefresh(msg.ok || msg.id === undefined ? msg.authExp : this.#authExp);
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
  // One entry per onEvent call, so the same function registered twice is
  // removed only by its own unsubscribe.
  #eventListeners = new Map<string, Set<{ fn: (data: any) => void }>>();
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

  /**
   * Listen for the server's ephemeral events on this subscription with the
   * given topic. Events are at-most-once: one sent while the client is
   * disconnected and resyncing is never delivered. Returns an unsubscribe
   * function.
   */
  onEvent<D = unknown>(topic: string, fn: (data: D) => void): () => void {
    let set = this.#eventListeners.get(topic);
    if (!set) this.#eventListeners.set(topic, (set = new Set()));
    const entry = { fn: fn as (data: any) => void };
    set.add(entry);
    return () => {
      set.delete(entry);
      if (set.size === 0 && this.#eventListeners.get(topic) === set) this.#eventListeners.delete(topic);
    };
  }

  _event(topic: string, data: unknown): void {
    for (const { fn } of [...(this.#eventListeners.get(topic) ?? [])]) {
      try {
        fn(data);
      } catch (err) {
        console.error("cometgo: event listener threw", this.name, topic, err);
      }
    }
  }

  stop(): void {
    this.#comet._unsubscribe(this);
    this.status = "ended";
  }

  _hasData(): boolean {
    return this.#init !== undefined;
  }

  _resumed(): void {
    // The session survived: data held is current, and the replay brings the rest.
    if (this.status === "resyncing") {
      this.status = "ready";
      this.#dirty = true;
      this._flush();
    }
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
    if (e.ops) target.applyAll(e.ops);
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

function lost(): CometError {
  return new CometError({ code: "CONNECTION_LOST", msg: "connection lost; outcome unknown" });
}

// A random (v4) UUID. crypto.randomUUID needs a secure context; getRandomValues does not.
function uuid(): string {
  const b = crypto.getRandomValues(new Uint8Array(16));
  b[6] = (b[6]! & 0x0f) | 0x40;
  b[8] = (b[8]! & 0x3f) | 0x80;
  const h = [...b].map((x) => x.toString(16).padStart(2, "0")).join("");
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

// Ends a sub the closed client will never serve, as a server end would.
function endClosed(sub: Subscription<any>): void {
  sub._entry({ s: sub.id, end: { code: "CANCELED", msg: "client closed" } });
  sub._flush();
}

function canceled(): CometError {
  return new CometError({ code: "CANCELED", msg: "call canceled" });
}

// url (default /_comet) resolved against the page, as ws(s), with route=key
// replacing any route in its query. The rest of the query keeps its bytes.
function routeUrl(url: string | undefined, key: string): string {
  const base = globalThis.location?.href;
  if (url === undefined && !base) throw new TypeError("cometgo: url is required outside a browser");
  const u = new URL(url ?? "/_comet", base);
  if (u.protocol === "https:") u.protocol = "wss:";
  else if (u.protocol === "http:") u.protocol = "ws:";
  if (u.protocol !== "ws:" && u.protocol !== "wss:") throw new TypeError(`cometgo: url ${u.href} is not ws(s) or http(s)`);
  const q = u.search.slice(1).split("&").filter((p) => p && p !== "route" && !p.startsWith("route="));
  q.push(`route=${encodeURIComponent(key)}`);
  u.search = q.join("&");
  return u.href;
}

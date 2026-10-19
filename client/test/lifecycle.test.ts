import { afterEach, beforeEach, mock, test } from "node:test";
import assert from "node:assert/strict";
import { connect, type Comet, type LifecycleEvent } from "../src/index.ts";

// The lifecycle hook and wake (M6 design §2.1, §2.5) against fake sockets
// that, like React Native's, do not fire onclose when closed.

class FakeWS {
  static all: FakeWS[] = [];
  readyState = 0;
  sent: any[] = [];
  closedWith?: number;
  onopen?: () => void;
  onmessage?: (ev: { data: string }) => void;
  onclose?: (ev: { code: number; reason: string }) => void;
  constructor() {
    FakeWS.all.push(this);
  }
  send(data: string): void {
    this.sent.push(JSON.parse(data));
  }
  close(code?: number): void {
    this.closedWith = code;
    if (this.readyState === 1) this.readyState = 2;
  }
  of(t: string): any[] {
    return this.sent.filter((m) => m.t === t);
  }
  open(): void {
    this.readyState = 1;
    this.onopen!();
  }
  recv(msg: unknown): void {
    this.onmessage!({ data: JSON.stringify(msg) });
  }
  drop(): void {
    this.readyState = 3;
    this.onclose!({ code: 1006, reason: "" });
  }
}

const welcome = { t: "welcome", session: "s_1", hb: 25000, user: null, caps: ["resume"] };

let comet: Comet;
let emit: (e: LifecycleEvent) => void;
let unsubscribed = 0;

beforeEach(() => {
  FakeWS.all = [];
  unsubscribed = 0;
  mock.timers.enable({ apis: ["setTimeout", "setInterval", "Date"], now: 1_000_000 });
});
afterEach(() => {
  comet?.close();
  mock.timers.reset();
});

const flush = async () => {
  for (let i = 0; i < 10; i++) await Promise.resolve();
};

async function start(extra: Record<string, unknown> = {}): Promise<FakeWS> {
  comet = connect({
    url: "ws://test/_comet",
    routeKey: "k",
    WebSocket: FakeWS as any,
    lifecycle: (on) => {
      emit = on;
      return () => unsubscribed++;
    },
    ...extra,
  });
  await flush();
  const ws = FakeWS.all.at(-1)!;
  ws.open();
  await flush();
  ws.recv(welcome);
  return ws;
}

test("a wake with no socket reconnects at once, without waiting for the backoff", async () => {
  const ws = await start();
  ws.drop(); // backoff scheduled
  emit("network");
  await flush();
  assert.equal(FakeWS.all.length, 2);
});

test("wakes while opening do not call the token source twice", async () => {
  let calls = 0;
  let give!: (t: string) => void;
  comet = connect({
    url: "ws://test/_comet",
    routeKey: "k",
    WebSocket: FakeWS as any,
    lifecycle: (on) => ((emit = on), () => {}),
    token: () => {
      calls++;
      return new Promise<string>((r) => (give = r));
    },
  });
  await flush();
  emit("foreground");
  emit("network");
  await flush();
  assert.equal(calls, 1);
  give("t");
  await flush();
  assert.equal(FakeWS.all.length, 1);
});

test("a network wake drops a connecting socket, and its late open is closed", async () => {
  comet = connect({ url: "ws://test/_comet", routeKey: "k", WebSocket: FakeWS as any, lifecycle: (on) => ((emit = on), () => {}) });
  await flush();
  const ws = FakeWS.all[0]!;
  emit("network");
  assert.equal(ws.closedWith, 4000);
  assert.equal(comet.status, "disconnected");
  ws.closedWith = undefined;
  ws.open(); // React Native opens a connecting socket despite close()
  assert.equal(ws.closedWith, 4000);
  assert.equal(ws.of("hello").length, 0);
});

test("a probe that gets nothing back drops the socket without waiting for onclose", async () => {
  const ws = await start();
  mock.timers.tick(2000);
  emit("foreground");
  assert.equal(ws.of("ping").length, 1);
  mock.timers.tick(5000);
  assert.equal(ws.closedWith, 4408);
  assert.equal(comet.status, "disconnected");
});

test("any message answers a probe, so a large replay does not drop the socket", async () => {
  const ws = await start();
  mock.timers.tick(2000);
  emit("foreground");
  mock.timers.tick(3000);
  ws.recv({ t: "tx", seq: 1, more: true, subs: [] }); // a replay part ahead of the pong
  mock.timers.tick(2500);
  assert.equal(ws.closedWith, undefined);
  assert.equal(comet.status, "connected");
});

test("probe limits: none right after a message, one at a time, none for 30 s after a probe drop", async () => {
  const ws = await start();
  emit("foreground"); // the welcome arrived just now
  assert.equal(ws.of("ping").length, 0);
  mock.timers.tick(2000);
  emit("foreground");
  emit("network");
  assert.equal(ws.of("ping").length, 1);
  mock.timers.tick(5000); // dropped
  emit("foreground"); // reconnects
  await flush();
  const next = FakeWS.all.at(-1)!;
  assert.notEqual(next, ws);
  next.open();
  await flush();
  next.recv({ ...welcome, hb: 120_000 }); // so the liveness check sends no ping of its own
  mock.timers.tick(2000);
  emit("network");
  assert.equal(next.of("ping").length, 0);
  mock.timers.tick(30_000);
  emit("network");
  assert.equal(next.of("ping").length, 1);
});

test("a dropped socket's late frames do not reach the client", async () => {
  const ws = await start();
  ws.recv({ t: "tx", seq: 5, subs: [] });
  mock.timers.tick(2000);
  emit("foreground");
  mock.timers.tick(5000); // probe drop
  ws.recv({ t: "tx", seq: 9, subs: [] }); // after close(), as React Native delivers
  emit("foreground");
  await flush();
  const next = FakeWS.all.at(-1)!;
  next.open();
  await flush();
  assert.equal(next.of("hello")[0].resume.seq, 5);
});

test("a wake runs an overdue token refresh", async () => {
  let calls = 0;
  const ws = await start({ token: () => (calls++, "t") });
  ws.recv({ ...welcome, t: "welcome", authExp: Math.floor(Date.now() / 1000) + 100 });
  const before = calls;
  mock.timers.setTime(Date.now() + 90_000); // the refresh timer never fired (app suspended)
  emit("foreground");
  await flush();
  assert.equal(calls, before + 1);
  assert.equal(ws.of("auth").length, 1);
});

test("closeInBackground closes at once, stays closed, and resumes on return", async () => {
  const ws = await start({ closeInBackground: true });
  emit("background");
  assert.equal(ws.closedWith, 4001); // not 1000/1001: the session stays resumable
  mock.timers.tick(60_000);
  assert.equal(FakeWS.all.length, 1);
  emit("foreground");
  await flush();
  const next = FakeWS.all.at(-1)!;
  next.open();
  await flush();
  assert.equal(next.of("hello")[0].resume.session, "s_1");
  next.recv({ ...welcome, resumed: true });
  assert.equal(comet.resumed, true);
});

test("the lifecycle is unsubscribed on close, and a throwing one does not break connect", async () => {
  await start();
  comet.close();
  assert.equal(unsubscribed, 1);
  const warn = mock.method(console, "warn", () => {});
  comet = connect({
    url: "ws://test/_comet",
    routeKey: "k",
    WebSocket: FakeWS as any,
    lifecycle: () => {
      throw new Error("no AppState");
    },
  });
  assert.equal(warn.mock.callCount(), 1);
  warn.mock.restore();
});

test("closeInBackground closes a socket created after the app went to the background", async () => {
  let give!: (t: string) => void;
  comet = connect({
    url: "ws://test/_comet",
    routeKey: "k",
    WebSocket: FakeWS as any,
    closeInBackground: true,
    lifecycle: (on) => ((emit = on), () => {}),
    token: () => new Promise<string>((r) => (give = r)),
  });
  await flush();
  emit("background"); // while the token resolves: no socket yet
  give("t");
  await flush();
  const ws = FakeWS.all[0]!;
  ws.open();
  assert.equal(ws.closedWith, 4001);
  assert.equal(ws.of("hello").length, 0);
});

test("close() while connecting: a socket that opens anyway is closed and sends nothing", async () => {
  comet = connect({ url: "ws://test/_comet", routeKey: "k", WebSocket: FakeWS as any, lifecycle: null });
  await flush();
  const ws = FakeWS.all[0]!;
  comet.close();
  ws.open(); // React Native opens a connecting socket despite close()
  assert.equal(ws.closedWith, 1000);
  assert.equal(ws.of("hello").length, 0);
});

test("closeInBackground: a pending reconnect does not open a socket in the background", async () => {
  let calls = 0;
  const ws = await start({ closeInBackground: true, token: () => (calls++, "t") });
  ws.drop(); // backoff pending
  const before = calls;
  emit("background");
  mock.timers.tick(60_000);
  await flush();
  assert.equal(FakeWS.all.length, 1);
  assert.equal(calls, before);
  emit("foreground");
  await flush();
  assert.equal(FakeWS.all.length, 2);
});

test("no refresh on wake after a welcome without authExp", async () => {
  let calls = 0;
  const ws = await start({ token: () => (calls++, "t") });
  ws.recv({ ...welcome, authExp: Math.floor(Date.now() / 1000) + 100 });
  ws.recv({ ...welcome }); // no authExp now
  const before = calls;
  mock.timers.setTime(Date.now() + 200_000);
  emit("foreground");
  await flush();
  assert.equal(calls, before);
});

test("a wake does not skip a goaway's delay", async () => {
  const ws = await start();
  ws.recv({ t: "goaway", retryInMs: 3000 });
  ws.drop();
  emit("network");
  await flush();
  assert.equal(FakeWS.all.length, 1);
  mock.timers.tick(3000);
  await flush();
  assert.equal(FakeWS.all.length, 2);
});

test("the browser default: a page loaded in a hidden tab with closeInBackground waits for visibility", async () => {
  const listeners: Record<string, () => void> = {};
  const doc = { visibilityState: "hidden", addEventListener: (t: string, f: () => void) => (listeners[t] = f), removeEventListener() {} };
  (globalThis as any).window = { addEventListener() {}, removeEventListener() {} };
  (globalThis as any).document = doc;
  try {
    comet = connect({ url: "ws://test/_comet", routeKey: "k", WebSocket: FakeWS as any, closeInBackground: true });
    await flush();
    assert.equal(FakeWS.all.length, 0);
    doc.visibilityState = "visible";
    listeners.visibilitychange!();
    await flush();
    assert.equal(FakeWS.all.length, 1);
  } finally {
    delete (globalThis as any).window;
    delete (globalThis as any).document;
  }
});

test("after a suspension the liveness check probes instead of dropping a healthy socket", async () => {
  const ws = await start({ lifecycle: null });
  mock.timers.tick(1000);
  mock.timers.setTime(Date.now() + 60_000); // suspended: no timer ran for a minute
  mock.timers.tick(1000);
  assert.equal(ws.closedWith, undefined);
  assert.equal(ws.of("ping").length, 1);
  ws.recv({ t: "pong" });
  mock.timers.tick(10_000);
  assert.equal(ws.closedWith, undefined);
  assert.equal(comet.status, "connected");
});

test("after a suspension a dead socket is dropped once the probe times out", async () => {
  const ws = await start({ lifecycle: null });
  mock.timers.tick(1000);
  mock.timers.setTime(Date.now() + 60_000);
  mock.timers.tick(1000);
  mock.timers.tick(5000);
  assert.equal(ws.closedWith, 4408);
});

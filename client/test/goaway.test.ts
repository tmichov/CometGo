import { afterEach, mock, test } from "node:test";
import assert from "node:assert/strict";
import { connect, type Comet } from "../src/index.ts";

// Graceful shutdown (PROTOCOL.md §7.3) against a scripted server.

class FakeWS {
  static all: FakeWS[] = [];
  readyState = 0;
  sent: any[] = [];
  onopen?: () => void;
  onmessage?: (ev: { data: string }) => void;
  onclose?: (ev: { code: number; reason: string }) => void;
  constructor() {
    FakeWS.all.push(this);
  }
  send(data: string): void {
    this.sent.push(JSON.parse(data));
  }
  close(): void {
    this.readyState = 3;
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
  closeWith(code: number): void {
    this.readyState = 3;
    this.onclose!({ code, reason: "" });
  }
}

const welcome = { t: "welcome", session: "s_1", hb: 25000, user: null, caps: ["resume"] };

let comet: Comet;
afterEach(() => {
  comet?.close();
  mock.timers.reset();
});

async function start(): Promise<FakeWS> {
  mock.timers.enable({ apis: ["setTimeout"] });
  FakeWS.all = [];
  comet = connect({ url: "ws://test/_comet", WebSocket: FakeWS as any });
  await Promise.resolve(); // #open awaits the token source
  const ws = FakeWS.all[0];
  ws.open();
  ws.recv(welcome);
  return ws;
}

test("after goaway the client reconnects after retryInMs, not the backoff", async () => {
  const ws = await start();
  ws.recv({ t: "goaway", reason: "restart", retryInMs: 1500 });
  ws.closeWith(4503);
  mock.timers.tick(1499);
  await Promise.resolve();
  assert.equal(FakeWS.all.length, 1, "reconnected early");
  mock.timers.tick(1);
  await Promise.resolve();
  assert.equal(FakeWS.all.length, 2);
});

test("a call in flight at goaway settles from its res", async () => {
  const ws = await start();
  const p = comet.call("m", {});
  const id = ws.of("call")[0].id;
  ws.recv({ t: "goaway", reason: "restart", retryInMs: 100 });
  ws.recv({ t: "res", seq: 1, id, ok: true, val: 42 });
  ws.closeWith(4503);
  assert.equal(await p, 42);
});

test("calls made after goaway wait for the next connection", async () => {
  const ws = await start();
  ws.recv({ t: "goaway", reason: "restart", retryInMs: 0 });
  const p = comet.call("m", {});
  assert.equal(ws.of("call").length, 0, "sent to a server that is going away");
  ws.closeWith(4503);
  mock.timers.tick(0);
  await Promise.resolve();
  const next = FakeWS.all[1];
  next.open();
  next.recv({ ...welcome, session: "s_2" });
  const [c] = next.of("call");
  assert.equal(c.m, "m");
  next.recv({ t: "res", seq: 1, id: c.id, ok: true, val: "done" });
  assert.equal(await p, "done");
});

test("the delay applies to one reconnect only", async () => {
  const ws = await start();
  ws.recv({ t: "goaway", reason: "restart", retryInMs: 4000 });
  ws.closeWith(4503);
  mock.timers.tick(4000);
  await Promise.resolve();
  const next = FakeWS.all[1];
  next.open();
  next.recv({ ...welcome, session: "s_2" });
  // A later drop without goaway uses the backoff again (at most 500 ms on
  // the first attempt), not the old retryInMs.
  next.closeWith(1006);
  mock.timers.tick(500);
  await Promise.resolve();
  assert.equal(FakeWS.all.length, 3);
});

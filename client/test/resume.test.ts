import { afterEach, test } from "node:test";
import assert from "node:assert/strict";
import { connect, type Comet } from "../src/index.ts";

// Session resume (PROTOCOL.md §7.2) against a scripted server.

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
  drop(): void {
    this.readyState = 3;
    this.onclose!({ code: 1006, reason: "" });
  }
}

const welcome = { t: "welcome", session: "s_1", hb: 25000, user: null, caps: ["resume"] };
const init = { cols: [["id", "i8"], ["title", "text"]], key: [0], ordered: true };

let comet: Comet;
afterEach(() => comet?.close());

async function connected(): Promise<FakeWS> {
  FakeWS.all = [];
  comet = connect({ url: "ws://test/_comet", WebSocket: FakeWS as any });
  await Promise.resolve(); // #open awaits the token source
  const ws = FakeWS.all[0];
  ws.open();
  ws.recv(welcome);
  return ws;
}

// Drops ws and waits for the client's reconnect (backoff is at most 500 ms here).
async function reconnect(ws: FakeWS): Promise<FakeWS> {
  const n = FakeWS.all.length;
  ws.drop();
  while (FakeWS.all.length === n) await new Promise((r) => setTimeout(r, 10));
  const next = FakeWS.all[n];
  next.open();
  return next;
}

test("hello offers resume with the last fully received seq and held subs", async () => {
  const ws = await connected();
  assert.deepEqual(ws.of("hello")[0].caps, ["resume"]);
  const sub = comet.subscribe("items");
  ws.recv({ t: "tx", seq: 1, subs: [{ s: sub.id, init, ops: [["i", "1", ["1", "a"], 0]], ready: true }] });
  ws.recv({ t: "tx", seq: 2, more: true, subs: [{ s: sub.id, ops: [["i", "2", ["2", "b"], 1]] }] }); // partial
  const ws2 = await reconnect(ws);
  assert.deepEqual(ws2.of("hello")[0].resume, { session: "s_1", seq: 1, subs: [sub.id] });
});

test("resumed: held calls are re-sent with their ids, nothing is rejected", async () => {
  const ws = await connected();
  const sub = comet.subscribe("items");
  ws.recv({ t: "tx", seq: 1, subs: [{ s: sub.id, init, ops: [], ready: true }] });
  let rejected: unknown;
  const call = comet.call("add", { x: 1 }).catch((e) => (rejected = e));
  assert.equal(ws.of("call")[0].id, 1);

  const ws2 = await reconnect(ws);
  assert.equal(sub.status, "resyncing");
  assert.equal(ws2.of("call").length, 0, "held call must wait for welcome");
  ws2.recv({ ...welcome, resumed: true });
  assert.deepEqual(ws2.of("call").map((c) => c.id), [1]);
  assert.equal(sub.status, "ready");

  ws2.recv({ t: "res", seq: 2, id: 1, ok: true, val: "v" });
  assert.equal(await call, "v");
  assert.equal(rejected, undefined);
});

test("not resumed: held calls get CONNECTION_LOST, calls sent on the new socket do not", async () => {
  const ws = await connected();
  const held = comet.call("add", 1);
  const ws2Promise = reconnect(ws);
  const queued = comet.call("add", 2); // made while disconnected
  const ws2 = await ws2Promise;
  assert.deepEqual(ws2.of("call").map((c) => c.id), [2], "queued call goes out pipelined");

  ws2.recv({ ...welcome, session: "s_2", resumed: false });
  await assert.rejects(held, { code: "CONNECTION_LOST" });
  ws2.recv({ t: "res", seq: 1, id: 2, ok: true, val: "ok" });
  assert.equal(await queued, "ok");
  assert.equal(ws2.of("call").length, 1, "held call never re-sent on a new session");
});

test("acks the last seq on the liveness tick", async () => {
  const ws = await connected();
  const sub = comet.subscribe("items");
  ws.recv({ t: "tx", seq: 3, subs: [{ s: sub.id, init, ops: [], ready: true }] });
  await new Promise((r) => setTimeout(r, 1100));
  assert.deepEqual(ws.of("ack"), [{ t: "ack", seq: 3 }]);
});

test("a server without resume keeps the v1-core behaviour", async () => {
  FakeWS.all = [];
  comet = connect({ url: "ws://test/_comet", WebSocket: FakeWS as any });
  await Promise.resolve();
  const ws = FakeWS.all[0];
  ws.open();
  ws.recv({ ...welcome, caps: [] });
  const call = comet.call("add", 1);
  ws.drop();
  await assert.rejects(call, { code: "CONNECTION_LOST" });
});

test("no ack goes out on a new socket before its welcome", async () => {
  const ws = await connected();
  const sub = comet.subscribe("items");
  ws.recv({ t: "tx", seq: 3, subs: [{ s: sub.id, init, ops: [], ready: true }] });
  const ws2 = await reconnect(ws);
  await new Promise((r) => setTimeout(r, 1100));
  assert.deepEqual(ws2.of("ack"), []);
});

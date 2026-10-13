import { afterEach, test } from "node:test";
import assert from "node:assert/strict";
import { connect, type Comet } from "../src/index.ts";

// Ephemeral server events (evt) against a scripted server.

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
  recv(msg: unknown): void {
    this.onmessage!({ data: JSON.stringify(msg) });
  }
  drop(): void {
    this.readyState = 3;
    this.onclose!({ code: 1006, reason: "" });
  }
}

const welcome = { t: "welcome", session: "s_1", hb: 25000, user: null, caps: ["resume", "evt"] };
const init = { cols: [["id", "i8"]], key: [0], ordered: true };

let comet: Comet;
afterEach(() => comet?.close());

async function connected(): Promise<FakeWS> {
  FakeWS.all = [];
  comet = connect({ url: "ws://test/_comet", WebSocket: FakeWS as any });
  await new Promise((r) => setImmediate(r)); // #open awaits the token source
  const ws = FakeWS.all[0];
  ws.readyState = 1;
  ws.onopen!();
  ws.recv(welcome);
  return ws;
}

test("hello offers the evt cap", async () => {
  const ws = await connected();
  assert.ok(ws.of("hello")[0].caps.includes("evt"));
});

test("onEvent receives events for its sub and topic only", async () => {
  const ws = await connected();
  const a = comet.subscribe("items");
  const b = comet.subscribe("other");
  const got: unknown[] = [];
  a.onEvent("progress", (d) => got.push(["a", d]));
  a.onEvent("done", (d) => got.push(["a-done", d]));
  b.onEvent("progress", (d) => got.push(["b", d]));
  ws.recv({ t: "evt", seq: 1, s: a.id, topic: "progress", data: { pct: 10 } });
  ws.recv({ t: "evt", seq: 2, s: b.id, topic: "progress", data: 20 });
  ws.recv({ t: "evt", seq: 3, s: a.id, topic: "other", data: 30 });
  assert.deepEqual(got, [["a", { pct: 10 }], ["b", 20]]);
});

test("an onEvent unsubscribe stops delivery; a throwing listener does not stop the others", async () => {
  const ws = await connected();
  const sub = comet.subscribe("items");
  const got: unknown[] = [];
  const orig = console.error;
  console.error = () => {};
  try {
    sub.onEvent("t", () => {
      throw new Error("boom");
    });
    const off = sub.onEvent("t", (d) => got.push(d));
    ws.recv({ t: "evt", seq: 1, s: sub.id, topic: "t", data: 1 });
    off();
    ws.recv({ t: "evt", seq: 2, s: sub.id, topic: "t", data: 2 });
  } finally {
    console.error = orig;
  }
  assert.deepEqual(got, [1]);
});

test("events for unknown or ended subs are dropped", async () => {
  const ws = await connected();
  const sub = comet.subscribe("items");
  const got: unknown[] = [];
  sub.onEvent("t", (d) => got.push(d));
  ws.recv({ t: "evt", seq: 1, s: 999, topic: "t", data: "unknown" });
  ws.recv({ t: "tx", seq: 2, subs: [{ s: sub.id, init, ops: [], ready: true }] });
  ws.recv({ t: "tx", seq: 3, subs: [{ s: sub.id, end: { code: "FORBIDDEN" } }] });
  ws.recv({ t: "evt", seq: 4, s: sub.id, topic: "t", data: "ended" });
  const stopped = comet.subscribe("items2");
  stopped.onEvent("t", (d) => got.push(d));
  stopped.stop();
  ws.recv({ t: "evt", seq: 5, s: stopped.id, topic: "t", data: "stopped" });
  assert.deepEqual(got, []);
});

test("an evt advances the seq a resume asks for", async () => {
  const ws = await connected();
  const sub = comet.subscribe("items");
  ws.recv({ t: "tx", seq: 1, subs: [{ s: sub.id, init, ops: [], ready: true }] });
  ws.recv({ t: "evt", seq: 2, s: sub.id, topic: "t", data: null });
  const n = FakeWS.all.length;
  ws.drop();
  while (FakeWS.all.length === n) await new Promise((r) => setTimeout(r, 10));
  const next = FakeWS.all[n];
  next.readyState = 1;
  next.onopen!();
  assert.equal(next.of("hello")[0].resume.seq, 2);
});

test("the same function registered twice is removed only by its own unsubscribe", async () => {
  const ws = await connected();
  const sub = comet.subscribe("items");
  let n = 0;
  const fn = () => n++;
  const offA = sub.onEvent("t", fn);
  sub.onEvent("t", fn);
  offA();
  ws.recv({ t: "evt", seq: 1, s: sub.id, topic: "t", data: null });
  assert.equal(n, 1);
});

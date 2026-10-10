import { afterEach, test } from "node:test";
import assert from "node:assert/strict";
import { connect, CometError, type Comet } from "../src/index.ts";

// Call cancellation (PROTOCOL.md §5) against a scripted server.

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

let comet: Comet;
afterEach(() => comet?.close());

async function start(open = true): Promise<FakeWS> {
  FakeWS.all = [];
  comet = connect({ url: "ws://test/_comet", WebSocket: FakeWS as any });
  await Promise.resolve(); // #open awaits the token source
  const ws = FakeWS.all[0];
  if (open) {
    ws.open();
    ws.recv(welcome);
  }
  return ws;
}

const isCode = (code: string) => (e: unknown) => e instanceof CometError && e.code === code;

test("an already-aborted signal rejects with CANCELED without sending", async () => {
  const ws = await start();
  await assert.rejects(comet.call("m", {}, { signal: AbortSignal.abort() }), isCode("CANCELED"));
  assert.equal(ws.of("call").length, 0);
});

test("aborting an unsent call rejects it locally and never sends it", async () => {
  const ws = await start(false); // not open yet: the call is queued
  const ac = new AbortController();
  const p = comet.call("m", {}, { signal: ac.signal });
  ac.abort();
  await assert.rejects(p, isCode("CANCELED"));
  ws.open();
  assert.equal(ws.of("call").length, 0);
});

test("aborting a sent call sends cancel and settles from the res", async () => {
  const ws = await start();
  const ac = new AbortController();
  let settled = false;
  const p = comet.call("m", {}, { signal: ac.signal }).finally(() => (settled = true));
  const id = ws.of("call")[0].id;
  ac.abort();
  assert.deepEqual(ws.of("cancel"), [{ t: "cancel", id }]);
  await Promise.resolve();
  assert.equal(settled, false, "settled before the server answered");
  ws.recv({ t: "res", seq: 1, id, ok: false, err: { code: "CANCELED", msg: "call canceled" } });
  await assert.rejects(p, isCode("CANCELED"));
});

test("a cancel that lost the race resolves with the real result", async () => {
  const ws = await start();
  const ac = new AbortController();
  const p = comet.call("m", {}, { signal: ac.signal });
  const id = ws.of("call")[0].id;
  ac.abort();
  ws.recv({ t: "res", seq: 1, id, ok: true, val: "committed" });
  assert.equal(await p, "committed");
});

test("aborting after the res does nothing", async () => {
  const ws = await start();
  const ac = new AbortController();
  const p = comet.call("m", {}, { signal: ac.signal });
  ws.recv({ t: "res", seq: 1, id: ws.of("call")[0].id, ok: true, val: 1 });
  assert.equal(await p, 1);
  ac.abort();
  assert.equal(ws.of("cancel").length, 0);
});

test("a call aborted while held for a resume is re-sent and then cancelled", async () => {
  const ws = await start();
  const ac = new AbortController();
  const p = comet.call("m", {}, { signal: ac.signal });
  const id = ws.of("call")[0].id;
  ws.drop();
  ac.abort();
  assert.equal(ws.of("cancel").length, 0);
  while (FakeWS.all.length === 1) await new Promise((r) => setTimeout(r, 10));
  const next = FakeWS.all[1];
  next.open();
  next.recv({ ...welcome, resumed: true });
  const sent = next.sent.filter((m) => m.t === "call" || m.t === "cancel").map((m) => [m.t, m.id]);
  assert.deepEqual(sent, [["call", id], ["cancel", id]]);
  next.recv({ t: "res", seq: 1, id, ok: false, err: { code: "CANCELED" } });
  await assert.rejects(p, isCode("CANCELED"));
});

test("a call aborted while held gets CONNECTION_LOST if the resume fails", async () => {
  const ws = await start();
  const ac = new AbortController();
  const p = comet.call("m", {}, { signal: ac.signal });
  ws.drop();
  ac.abort();
  while (FakeWS.all.length === 1) await new Promise((r) => setTimeout(r, 10));
  const next = FakeWS.all[1];
  next.open();
  next.recv({ ...welcome, session: "s_2", resumed: false });
  await assert.rejects(p, isCode("CONNECTION_LOST"));
  assert.equal(next.of("call").length + next.of("cancel").length, 0);
});

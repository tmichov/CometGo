import { afterEach, beforeEach, test } from "node:test";
import assert from "node:assert/strict";
import { connect, CometError, type Comet } from "../src/index.ts";

class FakeWS {
  static last: FakeWS;
  readyState = 0;
  sent: any[] = [];
  onopen?: () => void;
  onmessage?: (ev: { data: string }) => void;
  onclose?: (ev: { code: number; reason: string }) => void;
  constructor() {
    FakeWS.last = this;
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
  open(caps: string[] = ["ik"]): void {
    this.readyState = 1;
    this.onopen!();
    this.recv({ t: "welcome", hb: 25000, user: null, caps });
  }
  recv(msg: unknown): void {
    this.onmessage!({ data: JSON.stringify(msg) });
  }
  drop(): void {
    this.readyState = 3;
    this.onclose!({ code: 1006, reason: "" });
  }
}

let comet: Comet;

beforeEach(async () => {
  comet = connect({ url: "ws://test/_comet", WebSocket: FakeWS as any });
  await Promise.resolve(); // #open awaits the token source
  FakeWS.last.open();
});

afterEach(() => comet.close());

// Waits for the client's backoff timer to open the next socket.
async function reconnected(prev: FakeWS): Promise<FakeWS> {
  for (let i = 0; i < 200 && FakeWS.last === prev; i++) await new Promise((r) => setTimeout(r, 5));
  assert.notEqual(FakeWS.last, prev, "client did not reconnect");
  const ws = FakeWS.last;
  ws.open();
  return ws;
}

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

test("idempotent calls carry a generated UUID ik; plain calls carry none", () => {
  void comet.call("a", {}, { idempotent: true });
  void comet.call("b", {}, { idempotent: true });
  void comet.call("c", {});
  const [a, b, c] = FakeWS.last.of("call");
  assert.match(a.ik, UUID);
  assert.match(b.ik, UUID);
  assert.notEqual(a.ik, b.ik);
  assert.equal("ik" in c, false);
});

test("hello offers the ik cap", () => {
  assert.deepEqual(FakeWS.last.of("hello")[0].caps, ["ik"]);
});

test("an explicit ik is sent as given", () => {
  void comet.call("a", {}, { ik: "order-42" });
  assert.equal(FakeWS.last.of("call")[0].ik, "order-42");
});

test("on disconnect, a sent call with ik is re-sent with the same ik and resolves", async () => {
  const first = FakeWS.last;
  const p = comet.callWithStatus("pay", { amount: 5 }, { idempotent: true });
  const sent = first.of("call")[0];
  first.drop();

  const second = await reconnected(first);
  const resent = second.of("call");
  assert.equal(resent.length, 1);
  assert.equal(resent[0].ik, sent.ik);
  assert.equal(resent[0].m, "pay");
  assert.deepEqual(resent[0].args, { amount: 5 });

  second.recv({ t: "res", seq: 1, id: resent[0].id, ok: true, val: "r1" });
  assert.deepEqual(await p, { value: "r1", lag: false });
});

test("on disconnect, a sent call without ik is still rejected with CONNECTION_LOST and not re-sent", async () => {
  const first = FakeWS.last;
  const p = comet.call("pay", {});
  first.drop();
  await assert.rejects(p, (e: unknown) => e instanceof CometError && e.code === "CONNECTION_LOST");
  const second = await reconnected(first);
  assert.equal(second.of("call").length, 0);
});

test("against a server without the ik cap, a sent ik call is rejected with CONNECTION_LOST", async () => {
  // An older server ignores ik, so re-sending would run the call twice.
  comet.close();
  comet = connect({ url: "ws://test/_comet", WebSocket: FakeWS as any });
  await Promise.resolve();
  const first = FakeWS.last;
  first.open([]);
  const p = comet.call("pay", {}, { idempotent: true });
  first.drop();
  await assert.rejects(p, (e: unknown) => e instanceof CometError && e.code === "CONNECTION_LOST");
  const second = await reconnected(first);
  assert.equal(second.of("call").length, 0);
});

test("an ik call that got its result is not re-sent", async () => {
  const first = FakeWS.last;
  const p = comet.call("pay", {}, { idempotent: true });
  first.recv({ t: "res", seq: 1, id: first.of("call")[0].id, ok: true, val: 1 });
  assert.equal(await p, 1);
  first.drop();
  const second = await reconnected(first);
  assert.equal(second.of("call").length, 0);
});

test("close() rejects a pending ik call instead of leaving it pending", async () => {
  const first = FakeWS.last;
  const p = comet.call("pay", {}, { idempotent: true });
  comet.close();
  first.drop(); // the socket's close event after close()
  await assert.rejects(p, (e: unknown) => e instanceof CometError && e.code === "CONNECTION_LOST");
});

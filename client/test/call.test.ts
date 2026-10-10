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
  open(caps: string[] = ["ik"], user: unknown = null): void {
    this.connect();
    this.recv({ t: "welcome", hb: 25000, user, caps });
  }
  /** The socket opens; the client sends hello. No welcome yet. */
  connect(): void {
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

let comet: Comet;

beforeEach(async () => {
  comet = connect({ url: "ws://test/_comet", WebSocket: FakeWS as any });
  await Promise.resolve(); // #open awaits the token source
  FakeWS.last.open();
});

afterEach(() => comet.close());

// Waits for the client's backoff timer to create the next socket.
async function nextSocket(prev: FakeWS): Promise<FakeWS> {
  for (let i = 0; i < 200 && FakeWS.last === prev; i++) await new Promise((r) => setTimeout(r, 5));
  assert.notEqual(FakeWS.last, prev, "client did not reconnect");
  return FakeWS.last;
}

async function reconnected(prev: FakeWS, caps?: string[], user?: unknown): Promise<FakeWS> {
  const ws = await nextSocket(prev);
  ws.open(caps, user);
  return ws;
}

async function lostConnection(p: Promise<unknown>): Promise<void> {
  await assert.rejects(p, (e: unknown) => e instanceof CometError && e.code === "CONNECTION_LOST");
}

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

test("idempotent calls carry a generated UUID ik; plain calls carry none", () => {
  comet.call("a", {}, { idempotent: true }).catch(() => {}); // rejected by close() in afterEach
  comet.call("b", {}, { idempotent: true }).catch(() => {}); // rejected by close() in afterEach
  comet.call("c", {}).catch(() => {}); // rejected by close() in afterEach
  const [a, b, c] = FakeWS.last.of("call");
  assert.match(a.ik, UUID);
  assert.match(b.ik, UUID);
  assert.notEqual(a.ik, b.ik);
  assert.equal("ik" in c, false);
});

test("hello offers the ik cap", () => {
  assert.ok(FakeWS.last.of("hello")[0].caps.includes("ik"));
});

test("an explicit ik is sent as given", () => {
  comet.call("a", {}, { ik: "order-42" }).catch(() => {}); // rejected by close() in afterEach
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

test("a retry waits for welcome, and is rejected if the new server lacks the ik cap", async () => {
  // An older server ignores ik, so re-sending would run the call twice.
  const first = FakeWS.last;
  const p = comet.call("pay", {}, { idempotent: true });
  first.drop();
  const second = await nextSocket(first);
  second.connect();
  assert.equal(second.of("call").length, 0, "re-sent before welcome");
  second.recv({ t: "welcome", hb: 25000, user: null, caps: [] });
  await lostConnection(p);
  assert.equal(second.of("call").length, 0);
});

test("a retry is rejected when the user changed", async () => {
  const first = FakeWS.last;
  const p = comet.call("pay", {}, { idempotent: true });
  first.drop();
  const second = await reconnected(first, ["ik"], { id: "bob" });
  await lostConnection(p);
  assert.equal(second.of("call").length, 0);
});

test("an ik call sent before welcome is not retried", async () => {
  const first = FakeWS.last;
  first.drop();
  const second = await nextSocket(first);
  second.connect();
  const p = comet.call("pay", {}, { idempotent: true });
  assert.equal(second.of("call").length, 1);
  second.drop(); // no welcome: the user it ran as is unknown
  await lostConnection(p);
});

test("close() during backoff rejects a held ik call", async () => {
  const first = FakeWS.last;
  const p = comet.call("pay", {}, { idempotent: true });
  first.drop();
  comet.close();
  await lostConnection(p);
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

test("a retry more than an hour after the first send is rejected", async () => {
  // Past the server's result TTL a retry would run the method again.
  const first = FakeWS.last;
  const p = comet.call("pay", {}, { idempotent: true });
  first.drop();
  const second = await nextSocket(first);
  const now = Date.now;
  Date.now = () => now() + 61 * 60 * 1000;
  try {
    second.open();
  } finally {
    Date.now = now;
  }
  await lostConnection(p);
  assert.equal(second.of("call").length, 0);
});

// With session resume (§7.2) as well: a resumed session re-sends held calls
// by id; a session that cannot be resumed falls back to the ik retry.
async function resumableStart(): Promise<FakeWS> {
  comet.close();
  comet = connect({ url: "ws://test/_comet", WebSocket: FakeWS as any });
  await Promise.resolve();
  const ws = FakeWS.last;
  ws.connect();
  ws.recv({ t: "welcome", session: "s_1", hb: 25000, user: null, caps: ["resume", "ik"] });
  return ws;
}

test("a held ik call on a resumed session is re-sent with its id and key", async () => {
  const first = await resumableStart();
  const p = comet.call("pay", {}, { idempotent: true });
  const sent = first.of("call")[0];
  first.drop();
  const second = await nextSocket(first);
  second.connect();
  assert.equal(second.of("hello")[0].resume.session, "s_1");
  second.recv({ t: "welcome", session: "s_1", resumed: true, hb: 25000, user: null, caps: ["resume", "ik"] });
  const resent = second.of("call");
  assert.equal(resent.length, 1);
  assert.equal(resent[0].id, sent.id);
  assert.equal(resent[0].ik, sent.ik);
  second.recv({ t: "res", seq: 1, id: sent.id, ok: true, val: 7 });
  assert.equal(await p, 7);
});

test("when the resume fails, a held ik call is retried on the new session", async () => {
  const first = await resumableStart();
  const p = comet.call("pay", {}, { idempotent: true });
  const plain = comet.call("other", {});
  const sent = first.of("call")[0];
  first.drop();
  const second = await nextSocket(first);
  second.connect();
  second.recv({ t: "welcome", session: "s_2", resumed: false, hb: 25000, user: null, caps: ["resume", "ik"] });
  await lostConnection(plain);
  const resent = second.of("call");
  assert.equal(resent.length, 1);
  assert.equal(resent[0].ik, sent.ik);
  second.recv({ t: "res", seq: 1, id: resent[0].id, ok: true, val: "stored" });
  assert.equal(await p, "stored");
});

test("when the resume fails and the server lacks ik, a held ik call gets CONNECTION_LOST", async () => {
  const first = await resumableStart();
  const p = comet.call("pay", {}, { idempotent: true });
  first.drop();
  const second = await nextSocket(first);
  second.connect();
  second.recv({ t: "welcome", session: "s_2", resumed: false, hb: 25000, user: null, caps: ["resume"] });
  await lostConnection(p);
  assert.equal(second.of("call").length, 0);
});

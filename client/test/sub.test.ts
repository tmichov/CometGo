import { afterEach, beforeEach, mock, test } from "node:test";
import assert from "node:assert/strict";
import { connect, type Comet } from "../src/index.ts";

// How a subscription applies the server's tx entries (PROTOCOL.md §4).

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
  recv(msg: unknown): void {
    this.onmessage!({ data: JSON.stringify(msg) });
  }
}

interface Row {
  id: number;
  title: string;
}

const init = { cols: [["id", "i8"], ["title", "text"]], key: [0], ordered: true };
const ins = (rows: [number, string][]) => rows.map((r, i) => ["i", String(r[0]), r, i]);

let comet: Comet;
let ws: FakeWS;
let seq = 0;

beforeEach(async () => {
  comet = connect({ url: "ws://test/_comet", WebSocket: FakeWS as any });
  await Promise.resolve(); // #open awaits the token source
  ws = FakeWS.last;
  ws.readyState = 1;
  ws.onopen!();
  ws.recv({ t: "welcome", hb: 25000, user: null });
  seq = 0;
});

afterEach(() => comet.close());

function tx(...subs: unknown[]): void {
  ws.recv({ t: "tx", seq: ++seq, subs });
}

// A ready sub holding rows.
function ready(rows: [number, string][]) {
  const sub = comet.subscribe<Row>("todos");
  tx({ s: sub.id, init, ops: ins(rows), ready: true });
  return sub;
}

test("a reset over held rows keeps unchanged row objects and replaces changed ones", () => {
  const sub = ready([[1, "a"], [2, "b"], [3, "c"]]);
  const [a, b] = sub.rows;
  tx({ s: sub.id, reset: true, ops: ins([[1, "a"], [2, "B"]]), ready: true });
  assert.deepEqual(sub.rows.map((r) => r.title), ["a", "B"]);
  assert.equal(sub.rows[0], a, "unchanged row keeps its identity");
  assert.notEqual(sub.rows[1], b);
  assert.equal(sub.status, "ready");
});

test("an init over held rows with the same columns also keeps identity", () => {
  const sub = ready([[1, "a"]]);
  const [a] = sub.rows;
  tx({ s: sub.id, init, ops: ins([[1, "a"]]), ready: true });
  assert.equal(sub.rows[0], a);
});

test("an init with different columns replaces every row object", () => {
  const sub = ready([[1, "a"]]);
  const [a] = sub.rows;
  const renamed = { cols: [["id", "i8"], ["name", "text"]], key: [0], ordered: true };
  tx({ s: sub.id, init: renamed, ops: ins([[1, "a"]]), ready: true });
  assert.notEqual(sub.rows[0], a, "a cached object would carry the old column names");
  assert.deepEqual(sub.rows[0], { id: 1, name: "a" });
});

test("a split tx is applied as one change with the final state", () => {
  const sub = ready([[1, "a"], [2, "b"]]);
  let notified = 0;
  sub.subscribe(() => notified++);
  ws.recv({ t: "tx", seq: ++seq, more: true, subs: [{ s: sub.id, ops: [["u", "1", [1, "a1"]]] }] });
  ws.recv({ t: "tx", seq, more: true, subs: [{ s: sub.id, ops: [["i", "3", [3, "c"], 2]] }] });
  assert.equal(notified, 0, "nothing applied before the last part");
  assert.deepEqual(sub.rows.map((r) => r.title), ["a", "b"]);
  ws.recv({ t: "tx", seq, subs: [{ s: sub.id, ops: [["d", "2"], ["u", "1", [1, "a2"]]] }] });
  assert.equal(notified, 1);
  assert.deepEqual(sub.rows.map((r) => r.title), ["a2", "c"]);
});

test("an op that cannot be applied resyncs the sub, which ends up with the server's data", () => {
  const err = mock.method(console, "error", () => {});
  try {
    const sub = ready([[1, "a"]]);
    const old = sub.id;
    tx({ s: old, ops: [["u", "9", [9, "x"]]] }); // update of a key the client lacks
    assert.equal(err.mock.callCount(), 1);
    assert.deepEqual(ws.of("unsub"), [{ t: "unsub", s: old }]);
    const resub = ws.of("sub").at(-1);
    assert.notEqual(sub.id, old);
    assert.deepEqual(resub, { t: "sub", s: sub.id, name: "todos", args: null, have: true });
    assert.deepEqual(sub.rows.map((r) => r.title), ["a"], "held rows stay visible meanwhile");

    tx({ s: old, ops: [["u", "1", [1, "late"]]] }); // late data for the old id is dropped
    tx({ s: sub.id, init, ops: ins([[1, "a"], [9, "x"]]), ready: true });
    assert.deepEqual(sub.rows.map((r) => r.title), ["a", "x"]);
    assert.equal(sub.status, "ready");
  } finally {
    err.mock.restore();
  }
});

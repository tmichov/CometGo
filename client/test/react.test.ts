import { afterEach, beforeEach, test } from "node:test";
import assert from "node:assert/strict";
import { JSDOM } from "jsdom";

// React DOM needs a DOM before it is imported.
const dom = new JSDOM("<!doctype html><html><body></body></html>");
Object.assign(globalThis, { window: dom.window, document: dom.window.document, IS_REACT_ACT_ENVIRONMENT: true });

const { act, Activity, createElement: h, StrictMode, useState } = await import("react");
const { createRoot } = await import("react-dom/client");
const { connect } = await import("../src/index.ts");
const { CometProvider, useCall, useConnection, useSubscription } = await import("../src/react.ts");
type Comet = import("../src/index.ts").Comet;

// ---- fake WebSocket ----

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
}

let comet: Comet;
let ws: FakeWS;
let container: HTMLElement;
let root: ReturnType<typeof createRoot>;

beforeEach(async () => {
  comet = connect({ url: "ws://test/_comet", WebSocket: FakeWS as any });
  await Promise.resolve(); // #open awaits the token source
  ws = FakeWS.last;
  ws.readyState = 1;
  ws.onopen!();
  server({ t: "welcome", hb: 25000, user: null });
  container = document.createElement("div");
  root = createRoot(container);
});

afterEach(() => {
  act(() => root.unmount());
  comet.close();
});

function server(msg: unknown): void {
  act(() => ws.onmessage!({ data: JSON.stringify(msg) }));
}

const init = { cols: [["id", "i8"], ["title", "text"]], key: [0], ordered: true };
function snapshot(s: number, rows: [string, string][]) {
  return { s, init, ops: rows.map((r, i) => ["i", r[0], r, i]), ready: true };
}

async function tick(): Promise<void> {
  await act(() => new Promise((r) => setTimeout(r, 5)));
}

function text(): string[] {
  return [...container.querySelectorAll("li")].map((li) => li.textContent!);
}

interface Row {
  id: string;
  title: string;
}

function List({ name, args, log }: { name: string; args?: unknown; log?: number[] }) {
  const { rows, status } = useSubscription<Row>(name, args);
  log?.push(rows.length);
  return h("ul", { "data-status": status }, ...rows.map((r) => h("li", { key: r.id }, r.title)));
}

function render(el: any): void {
  act(() => root.render(h(CometProvider, { client: comet }, el)));
}

// ---- tests ----

test("rows render once the snapshot arrives", () => {
  render(h(List, { name: "todos.all" }));
  assert.deepEqual(ws.of("sub"), [{ t: "sub", s: 1, name: "todos.all", args: null }]);
  assert.equal(container.querySelector("ul")!.dataset.status, "loading");

  server({ t: "tx", seq: 1, subs: [snapshot(1, [["1", "Buy milk"], ["2", "Ship v1"]])] });
  assert.deepEqual(text(), ["Buy milk", "Ship v1"]);
  assert.equal(container.querySelector("ul")!.dataset.status, "ready");

  server({ t: "tx", seq: 2, subs: [{ s: 1, ops: [["u", "1", [1, "Buy oat milk"]]] }] });
  assert.deepEqual(text(), ["Buy oat milk", "Ship v1"]);
});

test("an args change keeps the old rows until the new sub is ready", async () => {
  const log: number[] = [];
  render(h(List, { name: "todos.page", args: { page: 1 }, log }));
  server({ t: "tx", seq: 1, subs: [snapshot(1, [["1", "a"], ["2", "b"]])] });
  assert.deepEqual(text(), ["a", "b"]);

  log.length = 0;
  render(h(List, { name: "todos.page", args: { page: 2 }, log }));
  await tick();
  assert.deepEqual(ws.of("sub").at(-1), { t: "sub", s: 2, name: "todos.page", args: { page: 2 } });
  assert.deepEqual(text(), ["a", "b"], "old rows stay while the new sub loads");
  assert.deepEqual(ws.of("unsub"), [], "old sub kept until the new one is ready");

  server({ t: "tx", seq: 2, subs: [snapshot(2, [["3", "c"]])] });
  assert.deepEqual(text(), ["c"]);
  await tick();
  assert.deepEqual(ws.of("unsub"), [{ t: "unsub", s: 1 }]);
  assert.ok(!log.includes(0), `no render with empty rows, got ${log}`);
});

test("args equal by value do not resubscribe", async () => {
  render(h(List, { name: "todos.page", args: { page: 1, size: 10 } }));
  render(h(List, { name: "todos.page", args: { size: 10, page: 1 } }));
  await tick();
  assert.equal(ws.of("sub").length, 1);
  assert.equal(ws.of("unsub").length, 0);
});

test("unmount sends unsub", async () => {
  render(h(List, { name: "todos.all" }));
  server({ t: "tx", seq: 1, subs: [snapshot(1, [["1", "a"]])] });
  render(null);
  await tick();
  assert.deepEqual(ws.of("unsub"), [{ t: "unsub", s: 1 }]);
});

test("StrictMode double-mount sends exactly one sub and no unsub", async () => {
  act(() => root.render(h(StrictMode, null, h(CometProvider, { client: comet }, h(List, { name: "todos.all" })))));
  await tick();
  assert.equal(ws.of("sub").length, 1);
  assert.equal(ws.of("unsub").length, 0);
  server({ t: "tx", seq: 1, subs: [snapshot(1, [["1", "a"]])] });
  assert.deepEqual(text(), ["a"]);
});

test("components with the same name and args share one sub", async () => {
  render(h("div", null, h(List, { name: "todos.all" }), h(List, { name: "todos.all" })));
  assert.equal(ws.of("sub").length, 1);
  server({ t: "tx", seq: 1, subs: [snapshot(1, [["1", "a"]])] });
  assert.deepEqual(text(), ["a", "a"]);

  render(h("div", null, h(List, { name: "todos.all" })));
  await tick();
  assert.equal(ws.of("unsub").length, 0, "still used by the remaining component");
  render(null);
  await tick();
  assert.equal(ws.of("unsub").length, 1);
});

test("useConnection follows status and user; useCall is stable", async () => {
  const calls: unknown[] = [];
  let setN!: (n: number) => void;
  function Conn() {
    const { status, user } = useConnection();
    const call = useCall();
    const [, set] = useState(0);
    setN = set;
    calls.push(call);
    return h("p", null, `${status}:${JSON.stringify(user)}`);
  }
  render(h(Conn));
  assert.equal(container.textContent, "connected:null");

  server({ t: "authRes", seq: 1, ok: true, user: { id: "42" } });
  assert.equal(container.textContent, 'connected:{"id":"42"}');

  act(() => setN(1));
  assert.ok(calls.length >= 2 && calls.every((c) => c === calls[0]), "call keeps its identity");

  const p = (calls[0] as Comet["call"])("todos.add", { title: "x" });
  assert.deepEqual(ws.of("call"), [{ t: "call", id: 1, m: "todos.add", args: { title: "x" } }]);
  server({ t: "res", seq: 2, id: 1, ok: true, val: { id: "7" } });
  assert.deepEqual(await p, { id: "7" });
});

test("a second component on a ready sub renders its rows on the first render", () => {
  render(h("div", null, h(List, { name: "todos.all" })));
  server({ t: "tx", seq: 1, subs: [snapshot(1, [["1", "a"]])] });
  const log: number[] = [];
  render(h("div", null, h(List, { name: "todos.all" }), h(List, { name: "todos.all", log })));
  assert.deepEqual(log, [1, ...log.slice(1)], `first render already has the row, got ${log}`);
  assert.ok(!log.includes(0));
  assert.equal(ws.of("sub").length, 1);
});

test("hiding and revealing a component never shows a stopped sub as ended", { skip: (Activity as unknown) === undefined ? "React has no <Activity> before 19.2" : false }, async () => {
  const el = (mode: "visible" | "hidden") => h(Activity, { mode, children: h(List, { name: "todos.all" }) });
  render(el("visible"));
  server({ t: "tx", seq: 1, subs: [snapshot(1, [["1", "a"]])] });
  render(el("hidden"));
  await tick();
  assert.deepEqual(ws.of("unsub"), [{ t: "unsub", s: 1 }]);
  render(el("visible"));
  const ul = container.querySelector("ul")!;
  assert.equal(ul.dataset.status, "loading");
  assert.deepEqual(ws.of("sub").at(-1), { t: "sub", s: 2, name: "todos.all", args: null });
  server({ t: "tx", seq: 2, subs: [snapshot(2, [["1", "a"]])] });
  assert.equal(container.querySelector("ul")!.dataset.status, "ready");
});

test("swapping the provider's client does not show the old client's stopped sub", async () => {
  render(h(List, { name: "todos.all" }));
  server({ t: "tx", seq: 1, subs: [snapshot(1, [["1", "a"]])] });
  const oldWs = ws;
  const comet2 = connect({ url: "ws://test/_comet", WebSocket: FakeWS as any });
  await Promise.resolve();
  const ws2 = FakeWS.last;
  ws2.readyState = 1;
  ws2.onopen!();
  try {
    act(() => root.render(h(CometProvider, { client: comet2 }, h(List, { name: "todos.all" }))));
    await tick();
    assert.equal(container.querySelector("ul")!.dataset.status, "loading");
    assert.deepEqual(oldWs.of("unsub"), [{ t: "unsub", s: 1 }]);
    assert.equal(ws2.of("sub").length, 1);
  } finally {
    act(() => root.render(null));
    comet2.close();
  }
});

test("a sub the server ended is requested again by a newly mounted component", () => {
  render(h("div", null, h(List, { name: "todos.mine" })));
  server({ t: "tx", seq: 1, subs: [{ s: 1, end: { code: "FORBIDDEN", msg: "log in" } }] });
  assert.equal(container.querySelector("ul")!.dataset.status, "ended");
  render(h("div", null, h(List, { name: "todos.mine" }), h(List, { name: "todos.mine" })));
  assert.equal(ws.of("sub").length, 2, "the new component re-subscribes");
});

test("hooks outside a provider throw", () => {
  const orig = console.error;
  console.error = () => {}; // React logs the uncaught render error
  try {
    assert.throws(() => act(() => root.render(h(List, { name: "x" }))), /CometProvider/);
  } finally {
    console.error = orig;
  }
});

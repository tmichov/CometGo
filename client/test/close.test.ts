import { test } from "node:test";
import assert from "node:assert/strict";
import { JSDOM } from "jsdom";

const dom = new JSDOM("<!doctype html><html><body></body></html>");
Object.assign(globalThis, { window: dom.window, document: dom.window.document });
const { connect } = await import("../src/index.ts");

class FakeWS {
  readyState = 0;
  send(): void {}
  close(): void {}
}

// Tracks the listeners currently registered on target, by type.
function track(target: EventTarget, live: Map<string, Set<unknown>>): void {
  const add = target.addEventListener.bind(target);
  const remove = target.removeEventListener.bind(target);
  target.addEventListener = (type: string, fn: any, opts?: any) => {
    if (!live.has(type)) live.set(type, new Set());
    live.get(type)!.add(fn);
    add(type, fn, opts);
  };
  target.removeEventListener = (type: string, fn: any, opts?: any) => {
    live.get(type)?.delete(fn);
    remove(type, fn, opts);
  };
}

test("close() removes the window and document listeners", () => {
  const live = new Map<string, Set<unknown>>();
  track(window, live);
  track(document, live);
  const comet = connect({ url: "ws://test/_comet", WebSocket: FakeWS as any });
  assert.equal(live.get("online")?.size, 1);
  assert.equal(live.get("visibilitychange")?.size, 1);
  comet.close();
  assert.equal(live.get("online")!.size, 0, "online listener still registered");
  assert.equal(live.get("visibilitychange")!.size, 0, "visibilitychange listener still registered");
});

import { test } from "node:test";
import assert from "node:assert/strict";
import { connect } from "../src/index.ts";

// Records the URL of every socket made; never opens.
class FakeWS {
  static urls: string[] = [];
  static all: FakeWS[] = [];
  readyState = 0;
  onclose?: (ev: { code: number; reason: string }) => void;
  constructor(url: string) {
    FakeWS.urls.push(url);
    FakeWS.all.push(this);
  }
  send(): void {}
  close(): void {
    this.readyState = 3;
  }
}

const settle = () => new Promise((r) => setTimeout(r, 0));

function route(url: string): string | null {
  return new URL(url).searchParams.get("route");
}

test("routeKey is sent as the route parameter", async () => {
  FakeWS.urls = [];
  const comet = connect({ url: "ws://test/_comet", WebSocket: FakeWS as any, routeKey: "acme" });
  await settle();
  comet.close();
  assert.equal(FakeWS.urls[0], "ws://test/_comet?route=acme");
});

test("an existing query is kept", async () => {
  FakeWS.urls = [];
  const comet = connect({ url: "ws://test/_comet?x=1&route=old", WebSocket: FakeWS as any, routeKey: "a b" });
  await settle();
  comet.close();
  const u = new URL(FakeWS.urls[0]!);
  assert.equal(u.searchParams.get("x"), "1");
  assert.deepEqual(u.searchParams.getAll("route"), ["a b"]);
});

test("the default key is random per client and kept across reconnects", async () => {
  FakeWS.urls = [];
  FakeWS.all = [];
  const a = connect({ url: "ws://test/_comet", WebSocket: FakeWS as any });
  const b = connect({ url: "ws://test/_comet", WebSocket: FakeWS as any });
  await settle();
  const [ka, kb] = FakeWS.urls.map(route);
  assert.ok(ka && kb);
  assert.notEqual(ka, kb);
  // Drop a's socket; it reconnects after its backoff.
  const n = FakeWS.urls.length;
  FakeWS.all[0]!.onclose!({ code: 1006, reason: "" });
  while (FakeWS.urls.length === n) await new Promise((r) => setTimeout(r, 10));
  a.close();
  b.close();
  assert.equal(route(FakeWS.urls.at(-1)!), ka);
});

import { test } from "node:test";
import assert from "node:assert/strict";

// A React Native-like environment, as measured on a device (M6 design §9):
// window without addEventListener, no document, crypto or location, and here
// no URL either (bare React Native's does not implement its setters).
(globalThis as any).window = {};
Object.defineProperty(globalThis, "crypto", { value: undefined, configurable: true });
delete (globalThis as any).URL;
const { connect } = await import("../src/index.ts");

class FakeWS {
  static urls: string[] = [];
  readyState = 0;
  constructor(url: string) {
    FakeWS.urls.push(url);
  }
  send(): void {}
  close(): void {
    this.readyState = 3;
  }
}

test("connect works without browser globals or URL", async () => {
  const comet = connect({ url: "https://api.test/_comet?sig=a%20b#x", routeKey: "k", WebSocket: FakeWS as any });
  await new Promise((r) => setImmediate(r));
  comet.close();
  assert.equal(FakeWS.urls[0], "wss://api.test/_comet?sig=a%20b&route=k");
});

test("a relative url, or none, needs a page", () => {
  assert.throws(() => connect({ url: "/_comet", routeKey: "k", WebSocket: FakeWS as any }), /relative/);
  assert.throws(() => connect({ routeKey: "k", WebSocket: FakeWS as any }), /url is required/);
});

test("the default route key needs crypto.getRandomValues (the React Native entry polyfills it)", () => {
  assert.throws(() => connect({ url: "ws://api.test/_comet", WebSocket: FakeWS as any }));
});

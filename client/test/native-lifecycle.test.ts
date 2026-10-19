import { test } from "node:test";
import assert from "node:assert/strict";
import { nativeLifecycle, withLifecycle, type NetInfoStateLike } from "../src/native-lifecycle.ts";
import type { LifecycleEvent } from "../src/index.ts";

// The React Native lifecycle against fake AppState and NetInfo, fed the
// event sequences measured on a device (M6 design §9).

function fakes(initial: string | null = "active") {
  let appFn!: (s: string) => void;
  let netFn!: (s: NetInfoStateLike) => void;
  let removed = 0;
  const appState = {
    currentState: initial,
    addEventListener: (_: "change", fn: (s: string) => void) => ((appFn = fn), { remove: () => removed++ }),
  };
  const netInfo = { addEventListener: (fn: (s: NetInfoStateLike) => void) => ((netFn = fn), () => removed++) };
  const events: LifecycleEvent[] = [];
  const unsubscribe = nativeLifecycle(appState, netInfo)((e) => events.push(e));
  return { app: (s: string) => appFn(s), net: (s: NetInfoStateLike) => netFn(s), events, unsubscribe, removed: () => removed };
}

const wifi: NetInfoStateLike = { type: "wifi", isConnected: true, details: { ipAddress: "192.168.0.104" } };
const cellular: NetInfoStateLike = { type: "cellular", isConnected: true, details: { ipAddress: null } };
const none: NetInfoStateLike = { type: "none", isConnected: false, details: null };

test("the identical events NetInfo fires at subscribe wake nothing", () => {
  const f = fakes();
  f.net(wifi);
  f.net(wifi);
  f.net(wifi);
  assert.deepEqual(f.events, []);
});

test("signal-strength, reachability and generation updates wake nothing; only the network change does", () => {
  const f = fakes();
  f.net(wifi);
  f.net({ ...wifi, details: { ipAddress: "192.168.0.104", strength: 40 } as any });
  f.net({ ...cellular, isInternetReachable: false } as any);
  f.net({ ...cellular, isInternetReachable: true } as any);
  f.net({ ...cellular, details: { ipAddress: null, cellularGeneration: "4g" } as any });
  assert.deepEqual(f.events, ["network"]); // only the move to cellular
});

test("Wi-Fi off: none, then cellular, wakes once, on cellular", () => {
  const f = fakes();
  f.net(wifi);
  f.net(none);
  assert.deepEqual(f.events, []); // going offline does not wake
  f.net(cellular);
  assert.deepEqual(f.events, ["network"]);
});

test("a new address on the same network type wakes", () => {
  const f = fakes();
  f.net(wifi);
  f.net({ ...wifi, details: { ipAddress: "10.0.0.7" } });
  assert.deepEqual(f.events, ["network"]);
});

test("an unknown connection (isConnected null) does not wake", () => {
  const f = fakes();
  f.net(wifi);
  f.net({ type: "unknown", isConnected: null, details: null });
  assert.deepEqual(f.events, []);
});

test("AppState: background, and foreground only when coming back", () => {
  const f = fakes();
  f.app("active"); // already active
  f.app("inactive"); // iOS, on the way out
  f.app("background");
  f.app("active");
  assert.deepEqual(f.events, ["background", "foreground"]);
});

test("an app started in the background reports it at once", () => {
  const f = fakes("background");
  assert.deepEqual(f.events, ["background"]);
  f.app("active");
  assert.deepEqual(f.events, ["background", "foreground"]);
});

test("unsubscribe removes both listeners", () => {
  const f = fakes();
  f.unsubscribe();
  assert.equal(f.removed(), 2);
});

test("withLifecycle: the native lifecycle unless the caller chose one", () => {
  const native = () => () => {};
  const own = () => () => {};
  assert.equal(withLifecycle({ url: "ws://x" }, native).lifecycle, native);
  assert.equal(withLifecycle({ url: "ws://x", lifecycle: undefined }, native).lifecycle, native); // spread config
  assert.equal(withLifecycle({ url: "ws://x", lifecycle: null }, native).lifecycle, null);
  assert.equal(withLifecycle({ url: "ws://x", lifecycle: own }, native).lifecycle, own);
});

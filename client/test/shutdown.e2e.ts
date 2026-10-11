// End-to-end graceful shutdown check, driven by TestClientShutdownEndToEnd
// (shutdown_test.go) against a real server: node client/test/shutdown.e2e.ts <ws url>.
// The test prints "inflight" once a call is running; the server is then
// shut down and replaced. The call must resolve, the socket must close with
// 4503, and the next socket must open about retryInMs later.

import assert from "node:assert/strict";
import { connect } from "../src/index.ts";

const url = process.argv[2];
const seen: any[] = [];
const opened: number[] = [];
const closes: { at: number; code: number }[] = [];

class RecordingWS extends WebSocket {
  constructor(u: string | URL, p?: string | string[]) {
    super(u, p);
    opened.push(Date.now());
  }
  set onmessage(fn: ((ev: MessageEvent) => void) | null) {
    super.onmessage = fn && ((ev: MessageEvent) => {
      seen.push(JSON.parse(ev.data as string));
      fn(ev);
    });
  }
  set onclose(fn: ((ev: CloseEvent) => void) | null) {
    super.onclose = fn && ((ev: CloseEvent) => {
      closes.push({ at: Date.now(), code: ev.code });
      fn(ev);
    });
  }
}

const comet = connect({ url, WebSocket: RecordingWS as unknown as typeof WebSocket });
const sub = comet.subscribe("items.all");
await sub.ready;

// slowAdd holds its transaction ~300 ms after inserting.
const call = comet.call<string>("slowAdd", { Owner: "e2e", Name: "during-shutdown" });
await new Promise((r) => setTimeout(r, 50));
console.log("inflight");

const id = await call; // CONNECTION_LOST if the res was not delivered first
assert.ok(id);

const goaway = seen.find((m) => m.t === "goaway");
assert.ok(goaway, "no goaway");
const resAt = seen.findIndex((m) => m.t === "res");
assert.ok(seen.indexOf(goaway) < resAt, "res before goaway: the call was not in flight");

// Wait for the replacement server's welcome.
const deadline = Date.now() + 10_000;
while (seen.filter((m) => m.t === "welcome").length < 2) {
  assert.ok(Date.now() < deadline, "never reconnected");
  await new Promise((r) => setTimeout(r, 20));
}
assert.equal(closes[0].code, 4503);
const waited = opened[1] - closes[0].at;
console.log(`retryInMs ${goaway.retryInMs}, reconnected after ${waited} ms`);
assert.ok(waited >= goaway.retryInMs - 20 && waited <= goaway.retryInMs + 1000, "reconnect delay");

comet.close();
console.log("ok");

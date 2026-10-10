// End-to-end resume check, driven by TestClientResumeEndToEnd (resume_test.go)
// against a real server: node client/test/resume.e2e.ts <ws url>.
// The socket is killed after a call is sent; the call's tx and res must
// arrive by replay, with no reset and no CONNECTION_LOST.

import assert from "node:assert/strict";
import { connect } from "../src/index.ts";

const url = process.argv[2];
const seen: any[] = [];

// A WebSocket the test can kill: once killed, nothing more is delivered, as
// if the network had dropped.
class KillableWS extends WebSocket {
  static all: KillableWS[] = [];
  #dead = false;
  constructor(u: string | URL, p?: string | string[]) {
    super(u, p);
    KillableWS.all.push(this);
  }
  set onmessage(fn: ((ev: MessageEvent) => void) | null) {
    super.onmessage = fn && ((ev: MessageEvent) => {
      if (this.#dead) return;
      seen.push(JSON.parse(ev.data as string));
      fn(ev);
    });
  }
  kill(): void {
    this.#dead = true;
    this.close();
  }
}

const comet = connect({ url, WebSocket: KillableWS as unknown as typeof WebSocket });
const sub = comet.subscribe<{ name: string }>("items.all");
await sub.ready;
assert.equal(sub.rows.length, 0);

const call = comet.call<string>("add", { Owner: "e2e", Name: "missed" });
KillableWS.all[0].kill(); // the call is on the wire; its tx and res are not

const id = await call; // rejects with CONNECTION_LOST if resume failed
assert.ok(id);
assert.deepEqual(sub.rows.map((r) => r.name), ["missed"]);
assert.equal(sub.status, "ready");

const welcomes = seen.filter((m) => m.t === "welcome");
assert.equal(welcomes.length, 2);
assert.equal(welcomes[1].resumed, true);
assert.equal(welcomes[1].session, welcomes[0].session);
const after = seen.slice(seen.indexOf(welcomes[1]) + 1);
const snapshots = after.filter((m) => m.t === "tx" && m.subs.some((e: any) => e.reset || e.init));
assert.deepEqual(snapshots, [], "resume sent a snapshot");
assert.equal(after.filter((m) => m.t === "res").length, 1);

comet.close();
console.log("ok");

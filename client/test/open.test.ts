import { test } from "node:test";
import assert from "node:assert/strict";
import { connect } from "../src/index.ts";

class FakeWS {
  static made = 0;
  readyState = 0;
  constructor() {
    FakeWS.made++;
  }
  send(): void {}
  close(): void {
    this.readyState = 3;
  }
}

test("close() while the token resolves opens no socket", async () => {
  FakeWS.made = 0;
  let give!: (t: string) => void;
  const token = new Promise<string>((r) => (give = r));
  const comet = connect({ url: "ws://test/_comet", WebSocket: FakeWS as any, token: () => token });
  comet.close();
  give("t");
  await token;
  await new Promise((r) => setTimeout(r, 0));
  assert.equal(FakeWS.made, 0);
});

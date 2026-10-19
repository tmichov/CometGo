import { test } from "node:test";
import assert from "node:assert/strict";
import { cometURL } from "../../cmd/cometgo/template/mobile/comet-url.ts";

// The mobile template's server URL (cmd/cometgo, M6 design §4.2).

test("EXPO_PUBLIC_COMET_URL wins, in development and in release", () => {
  assert.equal(cometURL({ url: "wss://api.test/_comet", port: "9000" }, "192.168.0.5:8081", true, "8080"), "wss://api.test/_comet");
  assert.equal(cometURL({ url: "wss://api.test/_comet" }, undefined, false, "8080"), "wss://api.test/_comet");
});

test("in development: Metro's host on the server's port", () => {
  assert.equal(cometURL({ port: "9000" }, "192.168.0.5:8081", true, "8080"), "ws://192.168.0.5:9000/_comet");
  assert.equal(cometURL({}, "localhost:8081", true, "8080"), "ws://localhost:8080/_comet");
  assert.equal(cometURL({}, "exp://10.0.0.2:8081/--/path", true, "8080"), "ws://10.0.0.2:8080/_comet");
});

test("a release build without a URL fails clearly", () => {
  assert.throws(() => cometURL({}, "localhost:8081", false, "8080"), /no server URL/);
  assert.throws(() => cometURL({}, undefined, true, "8080"), /no server URL/);
});

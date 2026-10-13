import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { type Op, RowSet, decoderFor } from "../src/store.ts";

// Vectors are produced by the Go server's diff (TestWriteDiffVectors), so this
// proves the client applies ops exactly as the server computes them.
interface Vec {
  ordered: boolean;
  old: unknown[][];
  new: unknown[][];
  ops: Op[] | null;
}
const vectors: Vec[] = JSON.parse(readFileSync(new URL("./diff_vectors.json", import.meta.url), "utf8"));

function load(rows: unknown[][]): RowSet<unknown> {
  const s = new RowSet<unknown>();
  for (const r of rows) s.apply(["i", r[0] as string, r]);
  return s;
}

test("client applies server diffs to the server's result", () => {
  assert.ok(vectors.length > 100);
  for (const [i, v] of vectors.entries()) {
    const s = load(v.old);
    for (const op of v.ops ?? []) s.apply(op);
    const want = v.new.map((r) => r[0] as string);
    const got = v.ordered ? s.keys : [...s.keys].sort();
    assert.deepEqual(got, v.ordered ? want : [...want].sort(), `vector ${i}`);
    for (const r of v.new) assert.deepEqual(s.rows.get(r[0] as string)!.cells, r, `vector ${i} row ${r[0]}`);
  }
});

// Random positional ops on a set large enough for applyAll's KeyList path,
// checked against applying the same ops one by one to a plain array.
function randomOps(keys: string[], n: number, seed: number): Op[] {
  let x = seed;
  const rnd = (m: number) => ((x = (x * 1103515245 + 12345) % 2147483648) % m);
  const live = [...keys];
  const ops: Op[] = [];
  for (let i = 0; i < n; i++) {
    const k = live[rnd(live.length)];
    switch (rnd(5)) {
      case 0:
        ops.push(["d", k]);
        live.splice(live.indexOf(k), 1);
        break;
      case 1: {
        const key = `new${seed}_${i}`;
        ops.push(["i", key, [key], rnd(live.length + 2)]); // may be past the end
        live.push(key);
        break;
      }
      case 2:
        ops.push(["u", k, [0, k], rnd(live.length)]);
        break;
      default:
        ops.push(["m", k, rnd(live.length)]);
    }
  }
  return ops;
}

test("applyAll on a large tx matches applying each op", () => {
  const keys = Array.from({ length: 3000 }, (_, i) => `k${i}`);
  for (let seed = 1; seed <= 20; seed++) {
    const ops = randomOps(keys, 1500, seed);
    const a = load(keys.map((k) => [k]));
    const b = load(keys.map((k) => [k]));
    a.applyAll(ops);
    for (const op of ops) b.apply(op);
    assert.deepEqual(a.keys, b.keys, `seed ${seed}`);
  }
});

test("applyAll of 4,500 moves over 10,000 rows is fast", () => {
  const keys = Array.from({ length: 10000 }, (_, i) => `k${i}`);
  const ops: Op[] = [];
  for (let i = 0; i < 4500; i++) ops.push(["m", `k${(i * 7919) % 10000}`, (i * 104729) % 10000]);
  const s = load(keys.map((k) => [k]));
  const start = performance.now();
  s.applyAll(ops);
  const ms = performance.now() - start;
  assert.equal(s.keys.length, 10000);
  assert.ok(ms < 50, `took ${ms.toFixed(1)} ms`);
});

test("adopt keeps object identity for unchanged rows", () => {
  const dec = (c: unknown[]) => ({ id: c[0], v: c[1] });
  const a = load([["a", 1], ["b", 2]]);
  const before = a.materialize(dec);
  const b = load([["b", 2], ["a", 9], ["c", 3]]);
  a.adopt(b);
  const after = a.materialize(dec);
  assert.deepEqual(a.keys, ["b", "a", "c"]);
  assert.equal(after[0], before[1], "unchanged row b reuses its object");
  assert.notEqual(after[1], before[0], "changed row a gets a new object");
});

test("decoders follow the type mapping", () => {
  assert.equal(decoderFor("i8")("9007199254740993"), "9007199254740993");
  assert.ok(Number.isNaN(decoderFor("f8")("NaN") as number));
  assert.equal(decoderFor("f8")("-Infinity"), -Infinity);
  assert.equal((decoderFor("tstz")("2026-10-10T08:00:00.123456Z") as Date).toISOString(), "2026-10-10T08:00:00.123Z");
  assert.equal(decoderFor("tstz")("infinity"), "infinity");
  assert.deepEqual(decoderFor("bytes")("AQID"), new Uint8Array([1, 2, 3]));
  assert.deepEqual(decoderFor("[]f8")(["NaN", 1, null]), [NaN, 1, null]);
});

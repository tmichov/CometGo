// Per-subscription row store: applies tx ops (PROTOCOL.md §4) and decodes
// wire values (§10). Kept free of networking so it can be tested alone.

export type Cell = unknown;
export type Op =
  | ["i", string, Cell[], number?]
  | ["u", string, (number | Cell)[], number?]
  | ["d", string]
  | ["m", string, number];

export interface SubInit {
  cols: [string, string][];
  key: number[];
  ordered: boolean;
  src?: string;
}

interface Entry<T> {
  cells: Cell[];
  obj?: T; // decoded lazily; dropped whenever cells change
}

/** Ordered keys plus their rows, as the server's result set. */
export class RowSet<T> {
  keys: string[] = [];
  rows = new Map<string, Entry<T>>();
  // Set only inside applyAll: keys live here instead of in `keys`.
  #list?: KeyList;

  /**
   * Applies one tx's ops. Each positional op on a plain array costs
   * O(rows), so a tx with many of them works on a KeyList instead.
   */
  applyAll(ops: Op[]): void {
    let positional = 0;
    for (const op of ops) if (op[0] !== "u" || op[3] !== undefined) positional++;
    if (positional < 64 || this.keys.length < 1024) {
      for (const op of ops) this.apply(op);
      return;
    }
    this.#list = new KeyList(this.keys);
    try {
      for (const op of ops) this.apply(op);
    } finally {
      this.keys = this.#list.flat();
      this.#list = undefined;
    }
  }

  apply(op: Op): void {
    switch (op[0]) {
      case "d": {
        this.remove(op[1]);
        this.rows.delete(op[1]);
        return;
      }
      case "i": {
        const [, key, cells, idx] = op;
        if (this.rows.has(key)) throw new Error(`insert of existing key ${key}`);
        this.rows.set(key, { cells });
        this.insert(idx, key);
        return;
      }
      case "u": {
        const [, key, patch, idx] = op;
        const prev = this.rows.get(key);
        if (!prev) throw new Error(`update of missing key ${key}`);
        const cells = prev.cells.slice();
        for (let i = 0; i < patch.length; i += 2) cells[patch[i] as number] = patch[i + 1];
        this.rows.set(key, { cells });
        if (idx !== undefined) {
          this.remove(key);
          this.insert(idx, key);
        }
        return;
      }
      case "m": {
        const [, key, idx] = op;
        this.remove(key);
        this.insert(idx, key);
        return;
      }
    }
  }

  private insert(idx: number | undefined, key: string): void {
    if (this.#list) this.#list.insert(idx, key);
    else if (idx === undefined) this.keys.push(key);
    else this.keys.splice(idx, 0, key);
  }

  private remove(key: string): void {
    if (this.#list) return this.#list.remove(key);
    const i = this.keys.indexOf(key);
    if (i < 0) throw new Error(`op on missing key ${key}`);
    this.keys.splice(i, 1);
  }

  /**
   * Adopts `next` as the new contents, keeping the existing entry (and so
   * the decoded object identity) for every row whose cells are unchanged.
   * This is what makes a reset flicker-free (§4.4).
   */
  adopt(next: RowSet<T>): void {
    for (const [key, entry] of next.rows) {
      const prev = this.rows.get(key);
      if (prev && cellsEqual(prev.cells, entry.cells)) next.rows.set(key, prev);
    }
    this.keys = next.keys;
    this.rows = next.rows;
  }

  materialize(decode: (cells: Cell[]) => T): T[] {
    return this.keys.map((k) => {
      const e = this.rows.get(k)!;
      return (e.obj ??= decode(e.cells));
    });
  }
}

// Keys in chunks of about sqrt(n), with each key's chunk in a map: remove
// and insert cost O(sqrt n) instead of O(n). Same results as the array ops
// in RowSet for any index >= 0, including one past the end (appends).
class KeyList {
  #chunks: string[][];
  #chunkOf = new Map<string, string[]>();
  #size: number;
  #max: number;

  constructor(keys: string[]) {
    this.#size = keys.length;
    const b = Math.max(32, Math.ceil(Math.sqrt(keys.length)));
    this.#max = 2 * b;
    this.#chunks = [];
    for (let i = 0; i < keys.length; i += b) this.#chunks.push(keys.slice(i, i + b));
    if (this.#chunks.length === 0) this.#chunks.push([]);
    for (const c of this.#chunks) for (const k of c) this.#chunkOf.set(k, c);
  }

  remove(key: string): void {
    const c = this.#chunkOf.get(key);
    if (!c) throw new Error(`op on missing key ${key}`);
    c.splice(c.indexOf(key), 1);
    this.#chunkOf.delete(key);
    this.#size--;
  }

  insert(idx: number | undefined, key: string): void {
    let ci = this.#chunks.length - 1;
    let at = this.#chunks[ci].length;
    if (idx !== undefined && idx < this.#size) {
      ci = 0;
      while (idx > this.#chunks[ci].length) idx -= this.#chunks[ci++].length;
      at = idx;
    }
    const c = this.#chunks[ci];
    c.splice(at, 0, key);
    this.#chunkOf.set(key, c);
    this.#size++;
    if (c.length > this.#max) {
      const tail = c.splice(c.length >> 1);
      this.#chunks.splice(ci + 1, 0, tail);
      for (const k of tail) this.#chunkOf.set(k, tail);
    }
  }

  flat(): string[] {
    return this.#chunks.flat();
  }
}

function cellsEqual(a: Cell[], b: Cell[]): boolean {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) {
    if (a[i] !== b[i] && JSON.stringify(a[i]) !== JSON.stringify(b[i])) return false;
  }
  return true;
}

// ---- value decoding (§10) ----

export type Decoder = (v: Cell) => unknown;

export function decoderFor(code: string): Decoder {
  if (code.startsWith("[]")) {
    const elem = decoderFor(code.slice(2));
    return (v) => (Array.isArray(v) ? v.map((x) => (x === null ? null : elem(x))) : v);
  }
  switch (code) {
    case "f8":
      return (v) => (typeof v === "string" ? Number(v) : v); // "NaN", "Infinity", "-Infinity"
    case "tstz":
    case "date":
      // "infinity"/"-infinity" stay strings; date stays "YYYY-MM-DD" (no timezone).
      return code === "tstz" ? (v) => (typeof v === "string" && !v.endsWith("infinity") ? new Date(v) : v) : (v) => v;
    case "bytes":
      return (v) => (typeof v === "string" ? base64ToBytes(v) : v);
    default:
      // i4, i8 (decimal string), num (decimal string), bool, text, enum, uuid, json, ts, raw.
      return (v) => v;
  }
}

function base64ToBytes(s: string): Uint8Array {
  const bin = atob(s);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

export function rowDecoder<T>(init: SubInit): (cells: Cell[]) => T {
  const cols = init.cols.map(([name, code]) => ({ name, dec: decoderFor(code) }));
  return (cells) => {
    const o: Record<string, unknown> = {};
    for (let i = 0; i < cols.length; i++) {
      const v = cells[i];
      o[cols[i].name] = v === null ? null : cols[i].dec(v);
    }
    return o as T;
  };
}

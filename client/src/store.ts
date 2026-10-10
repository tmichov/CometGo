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
        if (idx === undefined) this.keys.push(key);
        else this.keys.splice(idx, 0, key);
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
          this.keys.splice(idx, 0, key);
        }
        return;
      }
      case "m": {
        const [, key, idx] = op;
        this.remove(key);
        this.keys.splice(idx, 0, key);
        return;
      }
    }
  }

  private remove(key: string): void {
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

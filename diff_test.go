package cometgo

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"testing"
)

// applyOps mirrors the client's op semantics (client/src/store.ts) so the
// server diff can be checked against the same rules the client applies.
func applyOps(t *testing.T, r *result, ops []op) *result {
	t.Helper()
	keys := slices.Clone(r.keys)
	rows := make(map[string][]json.RawMessage, len(r.rows))
	for k, v := range r.rows {
		rows[k] = slices.Clone(v)
	}
	remove := func(k string) {
		i := slices.Index(keys, k)
		if i < 0 {
			t.Fatalf("op on missing key %q", k)
		}
		keys = slices.Delete(keys, i, i+1)
	}
	for _, o := range ops {
		switch o.kind {
		case opDelete:
			remove(o.key)
			delete(rows, o.key)
		case opInsert:
			if _, ok := rows[o.key]; ok {
				t.Fatalf("insert of existing key %q", o.key)
			}
			rows[o.key] = o.row
			if o.hasIdx {
				keys = slices.Insert(keys, o.idx, o.key)
			} else {
				keys = append(keys, o.key)
			}
		case opUpdate:
			row := rows[o.key]
			for i, c := range o.patch {
				row[c] = o.cells[i]
			}
			if o.hasIdx {
				remove(o.key)
				keys = slices.Insert(keys, o.idx, o.key)
			}
		case opMove:
			remove(o.key)
			keys = slices.Insert(keys, o.idx, o.key)
		}
	}
	return &result{keys: keys, rows: rows}
}

func mkResult(keys []string, val func(k string) string) *result {
	r := &result{keys: keys, rows: map[string][]json.RawMessage{}}
	for _, k := range keys {
		r.rows[k] = []json.RawMessage{quote(k), quote(val(k))}
	}
	return r
}

func sameResult(a, b *result) bool {
	if !slices.Equal(a.keys, b.keys) || len(a.rows) != len(b.rows) {
		return false
	}
	for k, ra := range a.rows {
		rb, ok := b.rows[k]
		if !ok || len(ra) != len(rb) {
			return false
		}
		for i := range ra {
			if string(ra[i]) != string(rb[i]) {
				return false
			}
		}
	}
	return true
}

func TestDiffMinimalCases(t *testing.T) {
	v := func(string) string { return "x" }
	cases := []struct {
		name     string
		old, new []string
		wantOps  int
	}{
		{"identical", []string{"a", "b", "c"}, []string{"a", "b", "c"}, 0},
		{"insert front", []string{"b", "c"}, []string{"a", "b", "c"}, 1},
		{"delete middle", []string{"a", "b", "c"}, []string{"a", "c"}, 1},
		{"first to last", []string{"a", "b", "c", "d"}, []string{"b", "c", "d", "a"}, 1},
		{"last to first", []string{"a", "b", "c", "d"}, []string{"d", "a", "b", "c"}, 1},
		{"swap", []string{"a", "b"}, []string{"b", "a"}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old, new := mkResult(c.old, v), mkResult(c.new, v)
			ops := diffResults(old, new, true)
			if len(ops) != c.wantOps {
				t.Fatalf("got %d ops, want %d: %v", len(ops), c.wantOps, ops)
			}
			if got := applyOps(t, old, ops); !sameResult(got, new) {
				t.Fatalf("apply mismatch: got %v want %v", got.keys, new.keys)
			}
		})
	}
}

func TestDiffPatchOnlyChangedColumns(t *testing.T) {
	old := mkResult([]string{"a"}, func(string) string { return "1" })
	new := mkResult([]string{"a"}, func(string) string { return "2" })
	ops := diffResults(old, new, true)
	if len(ops) != 1 || ops[0].kind != opUpdate || !slices.Equal(ops[0].patch, []int{1}) || ops[0].hasIdx {
		t.Fatalf("unexpected ops %+v", ops)
	}
	b, _ := json.Marshal(ops[0])
	if string(b) != `["u","a",[1,"2"]]` {
		t.Fatalf("wire form %s", b)
	}
}

// randomPair builds a random old/new result pair over a small key space so
// inserts, deletes, moves and patches all occur together.
func randomPair(rng *rand.Rand) (*result, *result) {
	n := rng.IntN(12)
	pool := make([]string, 16)
	for i := range pool {
		pool[i] = fmt.Sprintf("k%d", i)
	}
	pick := func() []string {
		p := slices.Clone(pool)
		rng.Shuffle(len(p), func(i, j int) { p[i], p[j] = p[j], p[i] })
		return p[:rng.IntN(n+1)]
	}
	ver := map[string]string{}
	for _, k := range pool {
		ver[k] = fmt.Sprint(rng.IntN(2))
	}
	old := mkResult(pick(), func(k string) string { return ver[k] })
	new := mkResult(pick(), func(k string) string {
		if rng.IntN(3) == 0 {
			return ver[k] + "*"
		}
		return ver[k]
	})
	return old, new
}

func TestDiffRandomOrdered(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 20000 {
		old, new := randomPair(rng)
		for _, ordered := range []bool{true, false} {
			got := applyOps(t, old, diffResults(old, new, ordered))
			if !ordered {
				// Unordered subs only promise the same row set.
				slices.Sort(got.keys)
				new = &result{keys: slices.Sorted(slices.Values(new.keys)), rows: new.rows}
			}
			if !sameResult(got, new) {
				t.Fatalf("case %d ordered=%v: old %v new %v got %v", i, ordered, old.keys, new.keys, got.keys)
			}
		}
	}
}

// TestWriteDiffVectors regenerates client/test/diff_vectors.json, which the
// TypeScript client applies with its own store to prove both sides agree on
// op semantics. Run with COMETGO_WRITE_VECTORS=1.
func TestWriteDiffVectors(t *testing.T) {
	if os.Getenv("COMETGO_WRITE_VECTORS") == "" {
		t.Skip("set COMETGO_WRITE_VECTORS=1 to regenerate")
	}
	type vec struct {
		Ordered bool                `json:"ordered"`
		Old     [][]json.RawMessage `json:"old"`
		New     [][]json.RawMessage `json:"new"`
		Ops     []op                `json:"ops"`
	}
	rows := func(r *result) [][]json.RawMessage {
		out := [][]json.RawMessage{}
		for _, k := range r.keys {
			out = append(out, r.rows[k])
		}
		return out
	}
	rng := rand.New(rand.NewPCG(7, 7))
	var vecs []vec
	for i := range 300 {
		old, new := randomPair(rng)
		ordered := i%4 != 0
		vecs = append(vecs, vec{ordered, rows(old), rows(new), diffResults(old, new, ordered)})
	}
	b, err := json.Marshal(vecs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("client/test/diff_vectors.json", b, 0o644); err != nil {
		t.Fatal(err)
	}
}

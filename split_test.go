package cometgo

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestSplitTx(t *testing.T) {
	mk := func(s uint64, n int) subEntry {
		e := subEntry{S: s, Init: &subInit{Cols: [][2]string{{"id", "i8"}}, Key: []int{0}}, Reset: true, Ready: true}
		for i := range n {
			e.Ops = append(e.Ops, json.RawMessage(fmt.Sprintf(`["i","%d",["%d"],%d]`, i, i, i)))
		}
		return e
	}
	in := []subEntry{mk(1, 500), mk(2, 3), mk(3, 500)}
	parts := splitTx(in, 4096)
	if len(parts) < 3 {
		t.Fatalf("expected several parts, got %d", len(parts))
	}
	got := map[uint64]int{}
	for pi, p := range parts {
		b, _ := json.Marshal(p)
		if len(b) > 4096+512 {
			t.Fatalf("part %d is %d bytes", pi, len(b))
		}
		for _, e := range p {
			got[e.S] += len(e.Ops)
		}
	}
	for _, e := range in {
		if got[e.S] != len(e.Ops) {
			t.Fatalf("sub %d: %d ops after split, want %d", e.S, got[e.S], len(e.Ops))
		}
	}
	// init/reset only on a sub's first piece, ready only on its last.
	seen := map[uint64]int{}
	for _, p := range parts {
		for _, e := range p {
			first := seen[e.S] == 0
			seen[e.S]++
			if (e.Init != nil) != first || e.Reset != first {
				t.Fatalf("sub %d piece %d: init/reset misplaced", e.S, seen[e.S])
			}
		}
	}
	for _, p := range parts {
		for _, e := range p {
			seen[e.S]--
			if e.Ready != (seen[e.S] == 0) {
				t.Fatalf("sub %d: ready misplaced", e.S)
			}
		}
	}
	if one := splitTx([]subEntry{mk(1, 2)}, 4096); len(one) != 1 {
		t.Fatalf("small tx split into %d parts", len(one))
	}
}

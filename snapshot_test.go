package cometgo

import (
	"context"
	"encoding/json"
	"testing"
)

// Subs of one observer that need a snapshot in the same tick share one
// encoding of its rows; Init and Reset stay per sub.
func TestSnapshotEncodedOncePerObserver(t *testing.T) {
	obs := &observer{
		pub:    &publication{},
		cols:   []colSpec{{name: "id", code: "i8"}},
		keyIdx: []int{0},
		res: &result{
			keys: []string{"1", "2"},
			rows: map[string][]json.RawMessage{"1": {json.RawMessage("1")}, "2": {json.RawMessage("2")}},
		},
	}
	e := &engine{}
	a := e.snapshotEntry(&subscription{s: 1, obs: obs})
	b := e.snapshotEntry(&subscription{s: 2, obs: obs, holds: true})
	if len(a.Ops) != 2 || &a.Ops[0] != &b.Ops[0] {
		t.Fatalf("snapshot ops not shared: %d ops, %p vs %p", len(a.Ops), &a.Ops[0], &b.Ops[0])
	}
	if a.S != 1 || b.S != 2 || a.Reset || !b.Reset || a.Init == b.Init {
		t.Fatalf("per-sub fields shared: a %+v b %+v", a, b)
	}
}

// A snapshot taken in a later tick is built from that tick's result, not
// reused from an earlier one.
func TestLaterSnapshotSeesNewRows(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, "INSERT INTO "+e.table+" (owner, name) VALUES ('alice', 'first')"); err != nil {
		t.Fatal(err)
	}
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	if ops := entries(a.next())[0]["ops"].([]any); len(ops) != 1 {
		t.Fatalf("first snapshot %v", ops)
	}
	if _, err := e.pool.Exec(ctx, "INSERT INTO "+e.table+" (owner, name) VALUES ('bob', 'second')"); err != nil {
		t.Fatal(err)
	}
	if ops := entries(a.next())[0]["ops"].([]any); len(ops) != 1 {
		t.Fatalf("diff %v", ops)
	}
	a.send(map[string]any{"t": "sub", "s": 2, "name": "items.all"})
	en := entries(a.next())
	if len(en) != 1 || en[0]["s"] != float64(2) || len(en[0]["ops"].([]any)) != 2 {
		t.Fatalf("second snapshot %v", en)
	}
}

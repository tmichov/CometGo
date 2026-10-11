package cometgo

import (
	"testing"
	"time"
)

// slowTick is long enough that waiting for the next tick is unmistakable.
const slowTick = 700 * time.Millisecond

// setupSlowTick adds a read-only "count" method, and ticks at most every
// slowTick, so a fenced call made right after a tick waits for the next.
func setupSlowTick(t *testing.T) *testEnv {
	return setupFull(t, func(c *Config) { c.MinTickInterval = slowTick }, func(env *testEnv, app *App) {
		Method(app, "count", func(c *Call, _ struct{}) (int, error) {
			var n int
			err := c.Tx.QueryRow(c.Context, "SELECT count(*) FROM "+env.table).Scan(&n)
			return n, err
		})
	})
}

// afterTick makes a write call, so the engine has just ticked.
func afterTick(t *testing.T, a *client, id int) {
	t.Helper()
	a.send(map[string]any{"t": "call", "id": id, "m": "add", "args": map[string]any{"Owner": "a", "Name": "n"}})
	if r := a.next(); r["t"] != "res" || r["ok"] != true {
		t.Fatalf("write %v", r)
	}
}

// timedCall returns the messages up to and including the res, and how long it took.
func timedCall(t *testing.T, a *client, call map[string]any) ([]map[string]any, time.Duration) {
	t.Helper()
	start := time.Now()
	a.send(call)
	got := a.collect(func(got []map[string]any) bool { return len(got) > 0 && got[len(got)-1]["t"] == "res" })
	return got, time.Since(start)
}

func TestReadOnlyCallSkipsFence(t *testing.T) {
	e := setupSlowTick(t)
	a, _ := e.dial(t, nil)
	afterTick(t, a, 1)
	got, d := timedCall(t, a, map[string]any{"t": "call", "id": 2, "m": "count"})
	if r := got[len(got)-1]; r["ok"] != true || r["val"] != float64(1) || r["lag"] != nil {
		t.Fatalf("res %v", r)
	}
	if d > slowTick/2 {
		t.Fatalf("read-only call took %v: it waited for a tick", d)
	}
}

// A write call is unchanged: it waits for the tick carrying its tx.
func TestWriteCallKeepsFence(t *testing.T) {
	e := setupSlowTick(t)
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	a.next()
	timedCall(t, a, map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "a", "Name": "n"}})
	got, d := timedCall(t, a, map[string]any{"t": "call", "id": 2, "m": "add", "args": map[string]any{"Owner": "a", "Name": "m"}})
	if len(got) != 2 || got[0]["t"] != "tx" {
		t.Fatalf("expected tx then res, got %v", got)
	}
	if d < slowTick/2 {
		t.Fatalf("write call took %v: it skipped the fence", d)
	}
}

// EmitAfterCommit with no writes keeps the fence: its evt precedes the res.
func TestEmitOnlyCallKeepsFence(t *testing.T) {
	e := setupFull(t, func(c *Config) { c.MinTickInterval = slowTick }, nil)
	a, _ := e.dial(t, evtCaps)
	subscribe(t, a, 1, "items.all")
	got, _ := timedCall(t, a, map[string]any{"t": "call", "id": 1, "m": "emitTwice"})
	if len(got) != 3 || got[0]["t"] != "evt" || got[1]["t"] != "evt" {
		t.Fatalf("expected evt, evt, res; got %v", got)
	}
}

// An idempotency replay keeps the fence, even of a call that wrote nothing.
func TestIdempotentReplayKeepsFence(t *testing.T) {
	e := setupSlowTick(t)
	a, _ := e.dial(t, nil)
	call := map[string]any{"t": "call", "id": 1, "m": "count", "ik": "ik-" + newID(8)}
	timedCall(t, a, call)
	afterTick(t, a, 2)
	call["id"] = 3
	got, d := timedCall(t, a, call)
	if r := got[len(got)-1]; r["ok"] != true {
		t.Fatalf("replay %v", r)
	}
	if d < slowTick/2 {
		t.Fatalf("replay took %v: it skipped the fence", d)
	}
}

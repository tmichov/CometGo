package cometgo

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func (e *testEnv) rowsNamed(t *testing.T, name string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), "SELECT count(*) FROM "+e.table+" WHERE name = $1", name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCancelDuringSleepRollsBack(t *testing.T) {
	e := setup(t)
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "call", "id": 1, "m": "sleepAdd", "args": map[string]any{"Name": "slow", "SleepMs": 5000}})
	time.Sleep(200 * time.Millisecond) // into pg_sleep
	start := time.Now()
	a.send(map[string]any{"t": "cancel", "id": 1})
	r := a.next()
	if r["t"] != "res" || r["id"] != float64(1) || r["ok"] != false || r["err"].(map[string]any)["code"] != CodeCanceled {
		t.Fatalf("expected CANCELED res, got %v", r)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("cancel took %v; the sleep was not interrupted", d)
	}
	if n := e.rowsNamed(t, "slow"); n != 0 {
		t.Fatalf("%d rows visible after cancel, want 0", n)
	}
	// The backend stopped too (pgx sends a cancel request when it drops a
	// connection over a cancelled context): nothing still sleeps or holds
	// the table's locks.
	deadline := time.Now().Add(time.Second)
	for {
		var n int
		if err := e.pool.QueryRow(context.Background(),
			"SELECT count(*) FROM pg_stat_activity WHERE state = 'active' AND query LIKE 'SELECT pg_sleep%'").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the cancelled query is still running in Postgres")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestContextOutlivesAFinishedCall(t *testing.T) {
	// c.Context is cancelled only by the client's cancel, not when the call ends.
	e := setup(t)
	got := make(chan error, 1)
	Method(e.app, "keepCtx", func(c *Call, _ struct{}) (struct{}, error) {
		ctx := c.Context
		go func() {
			time.Sleep(100 * time.Millisecond)
			got <- ctx.Err()
		}()
		return struct{}{}, nil
	})
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "call", "id": 1, "m": "keepCtx"})
	if r := a.next(); r["t"] != "res" || r["ok"] != true {
		t.Fatalf("res %v", r)
	}
	if err := <-got; err != nil {
		t.Fatalf("c.Context after the call: %v, want nil", err)
	}
}

func TestCancelRacingCommitNeverMismatches(t *testing.T) {
	e := setup(t)
	a, _ := e.dial(t, nil)
	var canceled, committed int
	for i := range 60 {
		name := fmt.Sprintf("race-%d", i)
		id := i + 1
		a.send(map[string]any{"t": "call", "id": id, "m": "sleepAdd", "args": map[string]any{"Name": name, "SleepMs": i % 3}})
		// Spread the cancel across the handler, the commit and after.
		time.Sleep(time.Duration(i%6) * time.Millisecond)
		a.send(map[string]any{"t": "cancel", "id": id})
		r := a.next()
		for r["t"] == "tx" { // none expected without subs, but harmless
			r = a.next()
		}
		if r["t"] != "res" || r["id"] != float64(id) {
			t.Fatalf("expected res %d, got %v", id, r)
		}
		n := e.rowsNamed(t, name)
		switch {
		case r["ok"] == true:
			committed++
			if n != 1 {
				t.Fatalf("call %d: ok res but %d rows", id, n)
			}
		case r["err"].(map[string]any)["code"] == CodeCanceled:
			canceled++
			if n != 0 {
				t.Fatalf("call %d: CANCELED but %d rows", id, n)
			}
		default:
			t.Fatalf("call %d: unexpected res %v", id, r)
		}
	}
	t.Logf("committed %d, canceled %d", committed, canceled)
}

func TestCancelOfUnknownOrFinishedCallIsIgnored(t *testing.T) {
	e := setup(t)
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "cancel", "id": 99})
	a.send(map[string]any{"t": "call", "id": 1, "m": "sleepAdd", "args": map[string]any{"Name": "done", "SleepMs": 0}})
	if r := a.next(); r["t"] != "res" || r["ok"] != true {
		t.Fatalf("res %v", r)
	}
	a.send(map[string]any{"t": "cancel", "id": 1})
	// No second res for id 1, and the connection still works.
	a.send(map[string]any{"t": "call", "id": 2, "m": "sleepAdd", "args": map[string]any{"Name": "next", "SleepMs": 0}})
	if r := a.next(); r["t"] != "res" || r["id"] != float64(2) || r["ok"] != true {
		t.Fatalf("expected res 2, got %v", r)
	}
	if n := e.rowsNamed(t, "done"); n != 1 {
		t.Fatalf("finished call's row: %d, want 1", n)
	}
}

func TestCancelAfterDisconnectIsNotImplied(t *testing.T) {
	// A dropped connection does not cancel a running call (§5): only cancel does.
	e := setup(t)
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "call", "id": 1, "m": "sleepAdd", "args": map[string]any{"Name": "orphan", "SleepMs": 300}})
	time.Sleep(100 * time.Millisecond)
	a.ws.CloseNow()
	deadline := time.Now().Add(5 * time.Second)
	for e.rowsNamed(t, "orphan") != 1 {
		if time.Now().After(deadline) {
			t.Fatal("call did not commit after its connection dropped")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

package cometgo

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// slowFnPub registers "slowfn", whose fn sleeps for sleep and records how
// many of its calls run at once. K 1 panics and K 2 is refused.
func slowFnPub(sleep time.Duration, running, peak *atomic.Int32) func(*testEnv, *App) {
	return func(env *testEnv, app *App) {
		Publish(app, "slowfn", PubOptions{Tables: []string{env.table}, Scope: ScopeShared},
			func(c *SubCtx, a struct{ K int }) (Query, error) {
				n := running.Add(1)
				defer running.Add(-1)
				for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
				}
				time.Sleep(sleep)
				switch a.K {
				case -1:
					panic("fn exploded")
				case -2:
					return Query{}, ErrForbidden
				}
				return Query{SQL: "SELECT id FROM " + env.table + " ORDER BY id"}, nil
			})
	}
}

// inject queues requests under one lock, so a single tick drains them all.
func inject(e *engine, reqs ...any) {
	e.mu.Lock()
	e.reqs = append(e.reqs, reqs...)
	e.mu.Unlock()
	e.signal()
}

func slowSub(c *conn, s uint64, k int) subReq {
	return subReq{c: c, s: s, name: "slowfn", args: json.RawMessage(fmt.Sprintf(`{"K":%d}`, k))}
}

// readSubs reads tx entries until every sub in want has an entry with
// ready or end, and returns the first such entry per sub.
func readSubs(t *testing.T, a *client, want int) map[float64]map[string]any {
	t.Helper()
	got := map[float64]map[string]any{}
	for len(got) < want {
		m := a.next()
		if m["t"] != "tx" {
			t.Fatalf("expected tx, got %v", m)
		}
		for _, en := range entries(m) {
			if _, seen := got[en["s"].(float64)]; !seen && (en["ready"] == true || en["end"] != nil) {
				got[en["s"].(float64)] = en
			}
		}
	}
	return got
}

// 50 subs whose fn takes 100 ms bind in about 50/FnWorkers fn times, not 50,
// and never more than FnWorkers fn calls run at once.
func TestConcurrentFnBindsQuickly(t *testing.T) {
	var running, peak atomic.Int32
	e := setupFull(t, nil, slowFnPub(100*time.Millisecond, &running, &peak))
	a, _ := e.dial(t, nil)
	c := e.app.liveConns()[0]
	var reqs []any
	for s := 1; s <= 50; s++ {
		reqs = append(reqs, slowSub(c, uint64(s), s))
	}
	start := time.Now()
	inject(e.app.engine, reqs...)
	for s, en := range readSubs(t, a, 50) {
		if en["ready"] != true {
			t.Fatalf("sub %v: %v", s, en)
		}
	}
	took := time.Since(start)
	t.Logf("50 subs bound in %s, peak %d fn calls at once", took, peak.Load())
	if took > 2*time.Second {
		t.Fatalf("50 subs took %s; sequential would be 5s", took)
	}
	if p := peak.Load(); p < 2 || p > 8 {
		t.Fatalf("peak %d concurrent fn calls, want 2..8 (FnWorkers)", p)
	}
}

// A panic or an error in one of a tick's concurrent fn calls ends only its
// own sub.
func TestConcurrentFnFailureEndsOnlyItsSub(t *testing.T) {
	var running, peak atomic.Int32
	e := setupFull(t, nil, slowFnPub(50*time.Millisecond, &running, &peak))
	a, _ := e.dial(t, nil)
	c := e.app.liveConns()[0]
	inject(e.app.engine, slowSub(c, 1, -1), slowSub(c, 2, -2), slowSub(c, 3, 3), slowSub(c, 4, 4))
	got := readSubs(t, a, 4)
	if end, _ := got[1]["end"].(map[string]any); end["code"] != CodeInternal {
		t.Fatalf("panicking fn: %v", got[1])
	}
	if !isForbiddenEnd(got[2]) {
		t.Fatalf("refusing fn: %v", got[2])
	}
	if got[3]["ready"] != true || got[4]["ready"] != true {
		t.Fatalf("other subs: %v %v", got[3], got[4])
	}
	if peak.Load() < 2 {
		t.Fatal("fn calls did not run concurrently")
	}
}

// An unsub in the same drain still wins over the sub it follows.
func TestConcurrentFnUnsubWins(t *testing.T) {
	var running, peak atomic.Int32
	e := setupFull(t, nil, slowFnPub(50*time.Millisecond, &running, &peak))
	a, _ := e.dial(t, nil)
	c := e.app.liveConns()[0]
	inject(e.app.engine, slowSub(c, 1, 1), unsubReq{c: c, s: 1}, slowSub(c, 2, 2))
	m := a.next()
	if en := entries(m); len(en) != 1 || en[0]["s"] != float64(2) || en[0]["ready"] != true {
		t.Fatalf("expected only sub 2's snapshot, got %v", m)
	}
}

// A rebind in the same drain as a sub binds that sub to the new identity.
func TestConcurrentFnRebindInSameDrain(t *testing.T) {
	e := setup(t)
	if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+e.table+" (owner, name) VALUES ('alice', 'a1'), ('bob', 'b1')"); err != nil {
		t.Fatal(err)
	}
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	c := e.app.liveConns()[0]
	id := uint64(9)
	inject(e.app.engine,
		subReq{c: c, s: 1, name: "items.mine", args: json.RawMessage(`{}`)},
		subReq{c: c, s: 2, name: "items.mine", args: json.RawMessage(`{}`)},
		rebindReq{c: c, ident: &Identity{UserID: "bob"}, after: &authResMsg{T: "authRes", ID: &id, OK: true}})
	m := a.next()
	en := entries(m)
	if len(en) != 2 {
		t.Fatalf("expected both subs' snapshots, got %v", m)
	}
	for _, x := range en {
		if ops := x["ops"].([]any); len(ops) != 1 || ops[0].([]any)[2].([]any)[1] != "b1" {
			t.Fatalf("sub %v not bound to bob: %v", x["s"], x)
		}
	}
	if r := a.next(); r["t"] != "authRes" || r["id"] != float64(9) {
		t.Fatalf("authRes %v", r)
	}
}

// Revalidate's concurrent re-checks end exactly the rejected subs, before
// the res of the method that caused them.
func TestConcurrentFnRevalidate(t *testing.T) {
	var members string
	e := setupFull(t, nil, dbTeam(t, &members))
	e.waitListening(t)
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	for s := 1; s <= 3; s++ {
		subscribe(t, a, s, "dbteam")
	}
	b, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "bob"}})
	subscribe(t, b, 1, "dbteam")

	a.send(map[string]any{"t": "call", "id": 1, "m": "leave", "args": map[string]any{"User": "alice"}})
	ended := 0
	for ended < 3 {
		m := a.next()
		if m["t"] != "tx" {
			t.Fatalf("expected the ends before res, got %v", m)
		}
		for _, en := range entries(m) {
			if !isForbiddenEnd(en) {
				t.Fatalf("unexpected entry %v", en)
			}
			ended++
		}
	}
	if r := a.next(); r["t"] != "res" || r["ok"] != true {
		t.Fatalf("res %v", r)
	}
	// Bob's sub is still live.
	b.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "x", "Name": "n"}})
	if en := entries(b.next()); en[0]["s"] != float64(1) || en[0]["ops"] == nil {
		t.Fatalf("bob's sub after alice left: %v", en)
	}
}

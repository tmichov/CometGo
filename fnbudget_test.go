package cometgo

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A burst of slow fn calls on one connection holds no tick past FnBudget:
// another connection's call is answered while they run, and every sub of
// the burst still binds.
func TestFnBurstDoesNotHoldOtherConns(t *testing.T) {
	var running, peak atomic.Int32
	e := setupFull(t, nil, slowFnPub(300*time.Millisecond, &running, &peak))
	e.waitListening(t) // the first LISTEN re-checks every sub, b's too
	a, _ := e.dial(t, nil)
	c := e.app.liveConns()[0]
	b, _ := e.dial(t, nil)
	subscribe(t, b, 1, "items.all")
	var reqs []any
	for s := 1; s <= 24; s++ { // 3 rounds of FnWorkers: about 900 ms of fn
		reqs = append(reqs, slowSub(c, uint64(s), s))
	}
	inject(e.app.engine, reqs...)
	time.Sleep(100 * time.Millisecond) // the burst is running
	start := time.Now()
	b.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "b", "Name": "n"}})
	expectOps(t, b.next(), 1)
	expectRes(t, b.next(), 1, false)
	if took := time.Since(start); took > 250*time.Millisecond {
		t.Fatalf("b's call took %s behind a's fn burst", took)
	}
	for s, en := range readSubs(t, a, 24) {
		if en["ready"] != true {
			t.Fatalf("sub %v: %v", s, en)
		}
	}
}

// A call made after a sub whose fn is still running waits for that sub's
// snapshot: the connection's fence is held by its carried request.
func TestCarriedSubHoldsItsConnsFence(t *testing.T) {
	var running, peak atomic.Int32
	e := setupFull(t, nil, slowFnPub(400*time.Millisecond, &running, &peak))
	e.waitListening(t)
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "slowfn", "args": map[string]any{"K": 1}})
	time.Sleep(100 * time.Millisecond) // carried by now
	a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "a", "Name": "n"}})
	ready := false
	for {
		m := nextWithin(t, a, 10*time.Second)
		if m["t"] == "res" {
			if !ready {
				t.Fatalf("res before the sub's snapshot: %v", m)
			}
			return
		}
		for _, en := range entries(m) {
			if en["ready"] == true {
				ready = true
			}
		}
	}
}

// A revalidation drained while a sub's fn is still running makes that fn
// run again: the first call may have checked access before the change, and
// no re-check reaches a sub that is not bound yet.
func TestRevalidateRerunsPendingFn(t *testing.T) {
	var members string
	e := setupFull(t, nil, func(env *testEnv, app *App) {
		dbTeam(t, &members)(env, app)
		// dbteam, but the check is followed by a slow step.
		Publish(app, "slowteam", PubOptions{Tables: []string{env.table}, Scope: ScopeShared},
			func(c *SubCtx, _ struct{}) (Query, error) {
				var ok bool
				if err := env.pool.QueryRow(c.Context, "SELECT EXISTS (SELECT 1 FROM "+members+" WHERE user_id = $1)", c.Identity.UserID).Scan(&ok); err != nil {
					return Query{}, err
				}
				time.Sleep(400 * time.Millisecond)
				if !ok {
					return Query{}, ErrForbidden
				}
				return Query{SQL: "SELECT id, name FROM " + env.table + " ORDER BY id"}, nil
			})
	})
	e.waitListening(t)
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	b, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "bob"}})
	a.send(map[string]any{"t": "sub", "s": 1, "name": "slowteam", "args": map[string]any{}})
	time.Sleep(100 * time.Millisecond) // alice's check passed; its fn sleeps
	b.send(map[string]any{"t": "call", "id": 1, "m": "leave", "args": map[string]any{"User": "alice"}})
	expectRes(t, nextWithin(t, b, 10*time.Second), 1, false)
	m := nextWithin(t, a, 10*time.Second)
	if en := entries(m); len(en) != 1 || !isForbiddenEnd(en[0]) {
		t.Fatalf("alice's sub should end FORBIDDEN, got %v", m)
	}
}

// While an identity change waits for its fn calls, the connection gets no
// data for its subs: the first thing after the auth is the rebound sub's
// reset, then authRes.
func TestCarriedRebindSendsNoOldData(t *testing.T) {
	var running, peak atomic.Int32
	e := setupFull(t, nil, slowFnPub(300*time.Millisecond, &running, &peak))
	e.waitListening(t)
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	a.send(map[string]any{"t": "sub", "s": 1, "name": "slowfn", "args": map[string]any{"K": 1}})
	if m := nextWithin(t, a, 10*time.Second); entries(m)[0]["ready"] != true {
		t.Fatalf("expected the snapshot, got %v", m)
	}
	a.send(map[string]any{"t": "auth", "id": 2, "token": "bob"})
	time.Sleep(100 * time.Millisecond) // the rebind is carried
	if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+e.table+" (owner, name) VALUES ('x', 'during')"); err != nil {
		t.Fatal(err)
	}
	m := nextWithin(t, a, 10*time.Second)
	if en := entries(m); m["t"] != "tx" || en[0]["reset"] != true {
		t.Fatalf("expected the rebound sub's reset first, got %v", m)
	}
	if m := nextWithin(t, a, 10*time.Second); m["t"] != "authRes" {
		t.Fatalf("expected authRes, got %v", m)
	}
}

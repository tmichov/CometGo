package cometgo

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Tests for per-connection fence release (M3 design §1). A deferrer keeps
// chosen publications' observers out of every tick's run list, as the tick
// budget and detach will.

type deferrer struct {
	mu   sync.Mutex
	pubs map[string]bool
	e    *engine
}

func (d *deferrer) hook(obs *observer) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pubs[obs.pub.name]
}

// set defers pub's observers, or lets them run again and wakes the engine.
func (d *deferrer) set(pub string, on bool) {
	d.mu.Lock()
	d.pubs[pub] = on
	d.mu.Unlock()
	d.e.signal()
}

func setupDeferred(t *testing.T, tweak func(*Config), extra func(*testEnv, *App)) (*testEnv, *deferrer) {
	t.Helper()
	d := &deferrer{pubs: map[string]bool{}}
	e := setupFull(t, tweak, func(env *testEnv, app *App) {
		if extra != nil {
			extra(env, app)
		}
		d.e = app.engine
		app.engine.deferObs = d.hook
	})
	// Events and fences travel through LISTEN; one sent before it is lost.
	e.waitListening(t)
	return e, d
}

// stream reads c's messages on a goroutine, so a test can check that
// nothing arrives for a while without closing the socket on a read timeout.
type stream struct {
	t  *testing.T
	ch chan map[string]any
}

func (c *client) stream(t *testing.T) *stream {
	s := &stream{t: t, ch: make(chan map[string]any, 256)}
	go func() {
		defer close(s.ch)
		for {
			_, b, err := c.ws.Read(context.Background())
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(b, &m) == nil && m["t"] != "pong" {
				s.ch <- m
			}
		}
	}()
	return s
}

func (s *stream) next() map[string]any {
	s.t.Helper()
	select {
	case m, ok := <-s.ch:
		if !ok {
			s.t.Fatal("socket closed")
		}
		return m
	case <-time.After(5 * time.Second):
		s.t.Fatal("no message within 5s")
		return nil
	}
}

// none fails if a message arrives within d.
func (s *stream) none(d time.Duration) {
	s.t.Helper()
	select {
	case m := <-s.ch:
		s.t.Fatalf("unexpected message %v", m)
	case <-time.After(d):
	}
}

func expectOps(t *testing.T, m map[string]any, s int) {
	t.Helper()
	if m["t"] != "tx" {
		t.Fatalf("expected a tx, got %v", m)
	}
	for _, en := range entries(m) {
		if en["s"] == float64(s) && en["ops"] != nil {
			return
		}
	}
	t.Fatalf("expected ops for sub %d, got %v", s, m)
}

func expectRes(t *testing.T, m map[string]any, id int, lag bool) {
	t.Helper()
	if m["t"] != "res" || m["id"] != float64(id) || m["ok"] != true || (m["lag"] == true) != lag {
		t.Fatalf("expected res %d with lag %v, got %v", id, lag, m)
	}
}

func addCall(id int, owner string) map[string]any {
	return map[string]any{"t": "call", "id": id, "m": "add", "args": map[string]any{"Owner": owner, "Name": "n"}}
}

// A fence is released as soon as its own connection is settled, while
// another connection's observer stays deferred.
func TestFenceReleasedWhileOtherConnDeferred(t *testing.T) {
	e, d := setupDeferred(t, nil, nil)
	b, _ := e.dial(t, nil)
	subscribe(t, b, 1, "items.all")
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	subscribe(t, a, 1, "items.mine")
	bs := b.stream(t)
	d.set("items.all", true)

	a.send(addCall(1, "alice"))
	expectOps(t, a.next(), 1)
	expectRes(t, a.next(), 1, false)
	bs.none(100 * time.Millisecond)

	d.set("items.all", false)
	expectOps(t, bs.next(), 1)
}

// A fence is held while its connection's own observer is deferred and
// stale from before the fence, and released by the tick that runs it.
func TestFenceHeldWhileOwnObserverDeferred(t *testing.T) {
	e, d := setupDeferred(t, nil, nil)
	a, _ := e.dial(t, nil)
	subscribe(t, a, 1, "items.all")
	as := a.stream(t)
	d.set("items.all", true)

	a.send(addCall(1, "x"))
	as.none(300 * time.Millisecond)
	d.set("items.all", false)
	expectOps(t, as.next(), 1)
	expectRes(t, as.next(), 1, false)
}

// A carried fence that reaches FenceTimeout answers with lag and leaves the
// carry list.
func TestCarriedFenceTimesOut(t *testing.T) {
	e, d := setupDeferred(t, func(c *Config) { c.FenceTimeout = 300 * time.Millisecond }, nil)
	a, _ := e.dial(t, nil)
	subscribe(t, a, 1, "items.all")
	as := a.stream(t)
	d.set("items.all", true)

	a.send(addCall(1, "x"))
	expectRes(t, as.next(), 1, true)
	eng := e.app.engine
	eng.mu.Lock()
	n := len(eng.fences)
	eng.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d fences still registered after the timeout", n)
	}
	d.set("items.all", false)
	expectOps(t, as.next(), 1)
	as.none(100 * time.Millisecond)
}

// A Match candidate the change cannot affect is fresh as of the snapshot,
// so it does not hold its connection's fence.
func TestUnaffectedCandidateDoesNotHoldFence(t *testing.T) {
	e := setupFull(t, func(c *Config) { c.FenceTimeout = time.Second }, func(env *testEnv, app *App) {
		Publish(app, "items.owned", PubOptions{Tables: []string{env.table}, Scope: ScopeShared},
			func(c *SubCtx, a struct{ Owner string }) (Query, error) {
				return Query{
					SQL:   "SELECT id, name FROM " + env.table + " WHERE owner = $1 ORDER BY id",
					Args:  []any{a.Owner},
					Match: map[string]any{"owner": a.Owner},
				}, nil
			})
	})
	e.waitListening(t)
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.owned", "args": map[string]any{"Owner": "zed"}})
	if m := a.next(); m["t"] != "tx" || entries(m)[0]["ready"] != true {
		t.Fatalf("snapshot %v", m)
	}
	runs := e.app.engine.runs.Load()
	a.send(addCall(1, "alice"))
	expectRes(t, a.next(), 1, false)
	if r := e.app.engine.runs.Load(); r != runs {
		t.Fatalf("the unaffected candidate ran (%d runs)", r-runs)
	}
}

// A sub still waiting for its first snapshot holds its connection's fence.
func TestSubAwaitingInitHoldsFence(t *testing.T) {
	e, d := setupDeferred(t, nil, nil)
	a, _ := e.dial(t, nil)
	as := a.stream(t)
	d.set("items.all", true)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	a.send(addCall(1, "x"))
	as.none(300 * time.Millisecond)

	d.set("items.all", false)
	m := as.next()
	if m["t"] != "tx" || entries(m)[0]["ready"] != true || len(entries(m)[0]["ops"].([]any)) != 1 {
		t.Fatalf("expected a snapshot with the call's row, got %v", m)
	}
	expectRes(t, as.next(), 1, false)
}

// A fence whose notification was lost while the listener reconnected gets
// the reconnect's all-dirty number, so it waits for its connection's
// observers to re-run, deferred ones included.
func TestReconnectFenceWaitsForDeferredObserver(t *testing.T) {
	e, d := setupDeferred(t, nil, nil)
	a, _ := e.dial(t, nil)
	subscribe(t, a, 1, "items.all")
	c := e.app.liveConns()[0]
	d.set("items.all", true)

	eng := e.app.engine
	token, f := eng.registerFence(c)
	eng.markCommitted(token)
	eng.listening() // the fence's notification was lost
	time.Sleep(300 * time.Millisecond)
	if released(f) {
		t.Fatal("released while the connection's observer is deferred")
	}
	d.set("items.all", false)
	for deadline := time.Now().Add(5 * time.Second); !released(f); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("not released once the observer ran")
		}
	}
}

// An event for a deferred observer is held until its data goes out, and
// still precedes the res of the call that emitted it.
func TestEvtHeldForDeferredObserver(t *testing.T) {
	e, d := setupDeferred(t, nil, nil)
	a, _ := e.dial(t, map[string]any{"caps": []string{"evt"}})
	subscribe(t, a, 1, "items.all")
	as := a.stream(t)
	d.set("items.all", true)

	a.send(map[string]any{"t": "call", "id": 1, "m": "addAndEmit", "args": map[string]any{"Owner": "x", "Name": "n"}})
	as.none(300 * time.Millisecond)
	d.set("items.all", false)
	expectOps(t, as.next(), 1)
	expectEvt(t, as.next(), 1, "added")
	expectRes(t, as.next(), 1, false)
}

// An event is not sent to a sub whose first snapshot is unsent, even when
// another sub of the same observer gets it.
func TestEvtSkipsSubAwaitingInit(t *testing.T) {
	e, d := setupDeferred(t, nil, nil)
	a, _ := e.dial(t, map[string]any{"caps": []string{"evt"}})
	subscribe(t, a, 1, "items.all")
	b, _ := e.dial(t, map[string]any{"caps": []string{"evt"}})
	as, bs := a.stream(t), b.stream(t)
	d.set("items.all", true)
	b.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	time.Sleep(100 * time.Millisecond)

	if err := e.app.Emit(context.Background(), "items.all", nil, "", "hi", 1); err != nil {
		t.Fatal(err)
	}
	expectEvt(t, as.next(), 1, "hi")
	bs.none(200 * time.Millisecond)
	d.set("items.all", false)
	if m := bs.next(); m["t"] != "tx" || entries(m)[0]["ready"] != true {
		t.Fatalf("expected b's snapshot, got %v", m)
	}
	bs.none(200 * time.Millisecond)
}

// authRes waits until a rebound sub's reset is sent, and two quick identity
// changes answer in order.
func TestAuthResWaitsForDeferredReset(t *testing.T) {
	e, d := setupDeferred(t, nil, nil)
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	subscribe(t, a, 1, "items.mine")
	as := a.stream(t)
	d.set("items.mine", true)

	a.send(map[string]any{"t": "auth", "id": 2, "token": "bob"})
	a.send(map[string]any{"t": "auth", "id": 3, "token": "alice"})
	as.none(300 * time.Millisecond)
	d.set("items.mine", false)
	if m := as.next(); m["t"] != "tx" || entries(m)[0]["reset"] != true {
		t.Fatalf("expected the reset, got %v", m)
	}
	for _, id := range []float64{2, 3} {
		if m := as.next(); m["t"] != "authRes" || m["id"] != id || m["ok"] != true {
			t.Fatalf("expected authRes %v, got %v", id, m)
		}
	}
}

// A coalescing connection forced out by a fence stays coalescing for a held
// sub whose observer is deferred, and the fence waits for that sub's reset.
func TestForcedCoalescingKeepsDeferredSubHeld(t *testing.T) {
	var other string
	e, d := setupDeferred(t, func(c *Config) { c.FenceTimeout = 10 * time.Second }, func(env *testEnv, app *App) {
		other = "other_" + newID(4)
		ctx := context.Background()
		if _, err := env.pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id bigserial PRIMARY KEY, name text NOT NULL)`, other)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { env.pool.Exec(context.Background(), "DROP TABLE "+other) })
		if err := InstallTriggers(ctx, env.pool, other); err != nil {
			t.Fatal(err)
		}
		Publish(app, "other.all", PubOptions{Tables: []string{other}, Scope: ScopeShared},
			func(c *SubCtx, _ struct{}) (Query, error) {
				return Query{SQL: "SELECT id, name FROM " + other + " ORDER BY id"}, nil
			})
	})
	ctx := context.Background()
	a, c, m := stalledClient(t, e)
	subscribe(t, a, 2, "other.all")
	e.waitCoalescing(t, c)
	// Sub 2 gets changes while coalescing, so it is held for a reset.
	if _, err := e.pool.Exec(ctx, "INSERT INTO "+other+" (name) VALUES ('o')"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	d.set("other.all", true)

	var id string
	if err := e.pool.QueryRow(ctx, "SELECT min(id)::text FROM "+e.table).Scan(&id); err != nil {
		t.Fatal(err)
	}
	a.send(map[string]any{"t": "call", "id": 1, "m": "bump", "args": map[string]any{"ID": id}})
	as := a.stream(t)
	sub2 := func(msg map[string]any) bool {
		if msg["t"] == "tx" {
			for _, en := range entries(msg) {
				if en["s"] == float64(2) {
					return true
				}
			}
		}
		return false
	}
	for !m.reset {
		msg := as.next()
		if msg["t"] == "res" || sub2(msg) {
			t.Fatalf("before sub 1's reset: %v", msg)
		}
		m.apply(t, msg)
	}
	as.none(300 * time.Millisecond)
	if !c.coalescing.Load() {
		t.Fatal("left coalescing with sub 2 still held")
	}

	d.set("other.all", false)
	msg := as.next()
	if !sub2(msg) {
		t.Fatalf("expected sub 2's reset, got %v", msg)
	}
	for _, en := range entries(msg) {
		if en["s"] == float64(2) && en["reset"] != true {
			t.Fatalf("expected sub 2's reset, got %v", en)
		}
	}
	expectRes(t, as.next(), 1, false)
	if c.coalescing.Load() {
		t.Fatal("still coalescing after every reset was sent")
	}
}

// Revalidate's end precedes the res of the method that caused it, while
// another connection's observer is deferred.
func TestRevalidateEndPrecedesResWithDeferral(t *testing.T) {
	var members string
	e, d := setupDeferred(t, nil, dbTeam(t, &members))
	b, _ := e.dial(t, nil)
	subscribe(t, b, 1, "items.all")
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	subscribe(t, a, 1, "dbteam")
	d.set("items.all", true)

	a.send(map[string]any{"t": "call", "id": 1, "m": "leave", "args": map[string]any{"User": "alice"}})
	if en := entries(a.next()); len(en) != 1 || !isForbiddenEnd(en[0]) {
		t.Fatalf("expected the end before res, got %v", en)
	}
	expectRes(t, a.next(), 1, false)
}

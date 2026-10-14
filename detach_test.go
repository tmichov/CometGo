package cometgo

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// logRec records log messages, for tests that check what was logged.
type logRec struct {
	mu   sync.Mutex
	msgs []string
}

func (l *logRec) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRec) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *logRec) WithGroup(string) slog.Handler            { return l }
func (l *logRec) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, r.Message)
	return nil
}

func (l *logRec) has(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range l.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// slowQ registers "slowq", whose query sleeps Ms milliseconds; K only
// separates observers.
func slowQ(env *testEnv, app *App) {
	Publish(app, "slowq", PubOptions{Tables: []string{env.table}, Scope: ScopeShared},
		func(c *SubCtx, a struct{ Ms, K int }) (Query, error) {
			return Query{
				SQL:  "SELECT id, owner, name FROM " + env.table + ", pg_sleep($1::float8 / 1000) ORDER BY id",
				Args: []any{a.Ms},
			}, nil
		})
}

func subSlowQ(t *testing.T, a *client, s, ms, k int) {
	t.Helper()
	a.send(map[string]any{"t": "sub", "s": s, "name": "slowq", "args": map[string]any{"Ms": ms, "K": k}})
	if m := nextWithin(t, a, 10*time.Second); m["t"] != "tx" || entries(m)[0]["ready"] != true {
		t.Fatalf("expected sub %d's snapshot, got %v", s, m)
	}
}

// rowNames collects the names in a tx's insert ops for sub s.
func rowNames(m map[string]any, s int) []string {
	var names []string
	if m["t"] != "tx" {
		return nil
	}
	for _, en := range entries(m) {
		if en["s"] != float64(s) {
			continue
		}
		ops, _ := en["ops"].([]any)
		for _, o := range ops {
			if o := o.([]any); o[0] == "i" {
				names = append(names, o[2].([]any)[2].(string))
			}
		}
	}
	return names
}

func insertName(t *testing.T, e *testEnv, name string) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+e.table+" (owner, name) VALUES ('x', $1)", name); err != nil {
		t.Fatal(err)
	}
}

func fastDetach(c *Config) { c.SlowObserverAfter = 300 * time.Millisecond }

// A 2 s observer detaches: calls on other connections are answered without
// waiting for it, and its result arrives later in its own tx with the
// write that made it run.
func TestDetachKeepsOtherConnsTicking(t *testing.T) {
	e := setupFull(t, fastDetach, slowQ)
	e.waitListening(t)
	a, _ := e.dial(t, nil)
	subSlowQ(t, a, 1, 2000, 0)
	b, _ := e.dial(t, nil)
	subscribe(t, b, 1, "items.all")

	start := time.Now()
	b.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "b", "Name": "from-b"}})
	expectOps(t, b.next(), 1)
	expectRes(t, b.next(), 1, false)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("b's call took %s behind a detached 2s query", took)
	}
	m := nextWithin(t, a, 10*time.Second)
	if names := rowNames(m, 1); len(names) != 1 || names[0] != "from-b" {
		t.Fatalf("a's detached result: %v", m)
	}
	if took := time.Since(start); took < 1500*time.Millisecond {
		t.Fatalf("a's result came after %s, before its 2s query could finish", took)
	}
}

// A sub that joins an observer while it is in flight gets a snapshot that
// contains a write committed before it subscribed, never the detached
// result.
func TestDetachJoinerSeesEarlierWrite(t *testing.T) {
	e := setupFull(t, fastDetach, slowQ)
	e.waitListening(t)
	a, _ := e.dial(t, nil)
	subSlowQ(t, a, 1, 1500, 0)
	insertName(t, e, "w1")
	time.Sleep(600 * time.Millisecond) // w1's run is detached by now
	insertName(t, e, "w2")
	c, _ := e.dial(t, nil)
	c.send(map[string]any{"t": "sub", "s": 1, "name": "slowq", "args": map[string]any{"Ms": 1500, "K": 0}})
	m := nextWithin(t, c, 10*time.Second)
	if names := rowNames(m, 1); len(names) != 2 || entries(m)[0]["ready"] != true {
		t.Fatalf("joiner's snapshot should hold w1 and w2: %v", m)
	}
	// a gets w1 from the detached run, then w2.
	var got []string
	for len(got) < 2 {
		got = append(got, rowNames(nextWithin(t, a, 10*time.Second), 1)...)
	}
	if got[0] != "w1" || got[1] != "w2" {
		t.Fatalf("a's updates %v", got)
	}
}

// A query that finishes right at the soft deadline races the detach. The
// next lane imports the snapshot before the detached transaction ends, so
// no tick fails and every result arrives.
func TestDetachAtTheDeadline(t *testing.T) {
	logs := &logRec{}
	e := setupFull(t, func(c *Config) {
		c.SlowObserverAfter = 200 * time.Millisecond
		c.Logger = slog.New(logs)
	}, slowQ)
	e.waitListening(t)
	a, _ := e.dial(t, nil)
	for k := 1; k <= 3; k++ {
		subSlowQ(t, a, k, 200, k)
	}
	for i := range 8 {
		insertName(t, e, "r")
		updated := map[float64]bool{}
		for len(updated) < 3 {
			for _, en := range entries(nextWithin(t, a, 10*time.Second)) {
				if en["ops"] != nil {
					updated[en["s"].(float64)] = true
				}
			}
		}
		if logs.has("tick snapshot failed") || logs.has("could not detach") {
			t.Fatalf("round %d: a tick failed or could not detach", i)
		}
	}
}

// An observer unsubscribed and re-created while its run is detached drops
// that run's result: only the new sub gets a snapshot.
func TestDetachedCompletionDroppedForRecreatedObserver(t *testing.T) {
	e := setupFull(t, fastDetach, slowQ)
	e.waitListening(t)
	a, _ := e.dial(t, nil)
	subSlowQ(t, a, 1, 1500, 0)
	insertName(t, e, "w")
	time.Sleep(500 * time.Millisecond) // detached by now
	a.send(map[string]any{"t": "unsub", "s": 1})
	a.send(map[string]any{"t": "sub", "s": 2, "name": "slowq", "args": map[string]any{"Ms": 1500, "K": 0}})
	as := a.stream(t)
	m := as.next()
	if en := entries(m); len(en) != 1 || en[0]["s"] != float64(2) || en[0]["ready"] != true || len(rowNames(m, 2)) != 1 {
		t.Fatalf("expected only sub 2's snapshot with the write, got %v", m)
	}
	as.none(2 * time.Second)
}

// A detached run that fails ends its subs, and a resumable session records
// the end, so a re-sent sub for that id is ignored.
func TestDetachedErrorEndsSubs(t *testing.T) {
	e := setupFull(t, func(c *Config) {
		c.SlowObserverAfter = 200 * time.Millisecond
		c.QueryTimeout = time.Second
	}, slowQ)
	a, _ := e.dial(t, resumeHello(nil))
	a.send(map[string]any{"t": "sub", "s": 1, "name": "slowq", "args": map[string]any{"Ms": 3000}})
	m := nextWithin(t, a, 10*time.Second)
	if en := entries(m); len(en) != 1 || en[0]["end"] == nil || en[0]["end"].(map[string]any)["code"] != CodeTimeout {
		t.Fatalf("expected a TIMEOUT end, got %v", m)
	}
	as := a.stream(t)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "slowq", "args": map[string]any{"Ms": 3000}})
	as.none(2500 * time.Millisecond)
}

// At the cap, an observer known to be slow is deferred rather than started,
// so it cannot block the tick; it runs once a slot frees.
func TestKnownSlowDeferredAtCap(t *testing.T) {
	e := setupFull(t, func(c *Config) {
		c.SlowObserverAfter = 200 * time.Millisecond
		c.MaxDetached = 1
	}, slowQ)
	e.waitListening(t)
	a, _ := e.dial(t, nil)
	subSlowQ(t, a, 1, 1000, 1) // each detaches once: known slow
	subSlowQ(t, a, 2, 1000, 2)
	b, _ := e.dial(t, nil)
	subscribe(t, b, 1, "items.all")

	insertName(t, e, "w")
	time.Sleep(300 * time.Millisecond) // one detached, the other deferred
	start := time.Now()
	b.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "b", "Name": "n"}})
	for m := b.next(); m["t"] != "res"; m = b.next() {
	}
	if took := time.Since(start); took > 600*time.Millisecond {
		t.Fatalf("b's call took %s: a known-slow observer blocked the tick", took)
	}
	updated := map[float64]bool{}
	for len(updated) < 2 {
		for _, en := range entries(nextWithin(t, a, 10*time.Second)) {
			if en["ops"] != nil {
				updated[en["s"].(float64)] = true
			}
		}
	}
}

// A known-slow observer's re-run is detached at once, so it does not hold
// the tick for SlowObserverAfter before detaching again.
func TestKnownSlowDetachesAtOnce(t *testing.T) {
	e := setupFull(t, func(c *Config) { c.SlowObserverAfter = 800 * time.Millisecond }, slowQ)
	e.waitListening(t)
	a, _ := e.dial(t, nil)
	subSlowQ(t, a, 1, 1500, 0) // its first run detaches at the deadline: known slow
	b, _ := e.dial(t, nil)
	subscribe(t, b, 1, "items.all")

	insertName(t, e, "w") // re-runs slowq
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	b.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "b", "Name": "n"}})
	for m := b.next(); m["t"] != "res"; m = b.next() {
	}
	if took := time.Since(start); took > 400*time.Millisecond {
		t.Fatalf("b's call took %s: the known-slow re-run held the tick", took)
	}
	var got []string
	for len(got) < 1 {
		got = append(got, rowNames(nextWithin(t, a, 10*time.Second), 1)...)
	}
	if got[0] != "w" {
		t.Fatalf("a's detached result %v", got)
	}
}

// A known-slow observer whose detached run finishes under the deadline is
// no longer known slow: with the only slot taken, it runs on the tick
// instead of waiting for the slot.
func TestKnownSlowClearedByFastRun(t *testing.T) {
	e := setupFull(t, func(c *Config) {
		c.SlowObserverAfter = 300 * time.Millisecond
		c.MaxDetached = 1
	}, func(env *testEnv, app *App) {
		slowQ(env, app)
		// Slow only while a row named "slow" exists.
		Publish(app, "toggleq", PubOptions{Tables: []string{env.table}, Scope: ScopeShared},
			func(c *SubCtx, _ struct{}) (Query, error) {
				return Query{SQL: "SELECT id, owner, name FROM " + env.table +
					", pg_sleep(CASE WHEN EXISTS (SELECT 1 FROM " + env.table + " WHERE name = 'slow') THEN 1 ELSE 0 END) ORDER BY id"}, nil
			})
	})
	e.waitListening(t)
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "toggleq", "args": map[string]any{}})
	if m := nextWithin(t, a, 10*time.Second); m["t"] != "tx" || entries(m)[0]["ready"] != true {
		t.Fatalf("expected toggleq's snapshot, got %v", m)
	}
	waitNames := func(want string) {
		t.Helper()
		for {
			if names := rowNames(nextWithin(t, a, 10*time.Second), 1); len(names) > 0 && names[len(names)-1] == want {
				return
			}
		}
	}
	insertName(t, e, "slow") // a 1 s run, detached: known slow
	waitNames("slow")
	if _, err := e.pool.Exec(context.Background(), "UPDATE "+e.table+" SET name = 'fast' WHERE name = 'slow'"); err != nil {
		t.Fatal(err)
	}
	for { // detached at once, but fast: no longer known slow
		m := nextWithin(t, a, 10*time.Second)
		if m["t"] == "tx" && len(entries(m)) > 0 && entries(m)[0]["ops"] != nil {
			break
		}
	}

	b, _ := e.dial(t, nil)
	b.send(map[string]any{"t": "sub", "s": 1, "name": "slowq", "args": map[string]any{"Ms": 3000}})
	time.Sleep(500 * time.Millisecond) // its first run has detached and holds the only slot
	insertName(t, e, "x")
	start := time.Now()
	waitNames("x")
	if took := time.Since(start); took > time.Second {
		t.Fatalf("toggleq's update took %s: it still waited for a detach slot", took)
	}
}

// With detaching off, a slow query holds its tick as before, and Start
// warns about it.
func TestMaxDetachedOffBlocksTick(t *testing.T) {
	logs := &logRec{}
	e := setupFull(t, func(c *Config) {
		c.SlowObserverAfter = 200 * time.Millisecond
		c.MaxDetached = -1
		c.Logger = slog.New(logs)
	}, slowQ)
	if !logs.has("will not be detached") {
		t.Fatal("no warning at Start")
	}
	e.waitListening(t)
	a, _ := e.dial(t, nil)
	subSlowQ(t, a, 1, 1000, 0)
	b, _ := e.dial(t, nil)
	subscribe(t, b, 1, "items.all")

	insertName(t, e, "w")
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	b.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "b", "Name": "n"}})
	for m := b.next(); m["t"] != "res"; m = b.next() {
	}
	if took := time.Since(start); took < 600*time.Millisecond {
		t.Fatalf("b's call took %s: the slow query did not hold the tick", took)
	}
	if names := rowNames(nextWithin(t, a, 10*time.Second), 1); len(names) == 0 {
		t.Fatal("a got no update")
	}
}

// A sub rebound by an identity change while its shared observer is in
// flight is a joiner: its reset comes from the next run, which holds a
// write committed before the rebind, never from the detached result.
func TestDetachRebindIsJoiner(t *testing.T) {
	e := setupFull(t, fastDetach, slowQ)
	e.waitListening(t)
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	subSlowQ(t, a, 1, 1500, 0)
	// b keeps the observer alive across a's rebind, so a rebinds to it.
	b, _ := e.dial(t, nil)
	subSlowQ(t, b, 1, 1500, 0)
	insertName(t, e, "w1")
	time.Sleep(600 * time.Millisecond) // w1's run is detached by now
	insertName(t, e, "w2")
	a.send(map[string]any{"t": "auth", "id": 2, "token": "bob"})
	for {
		m := nextWithin(t, a, 10*time.Second)
		if m["t"] == "authRes" {
			t.Fatal("authRes before the rebound sub's reset")
		}
		en := entries(m)
		if en[0]["reset"] == true {
			if names := rowNames(m, 1); len(names) != 2 {
				t.Fatalf("rebound sub's reset should hold w1 and w2: %v", names)
			}
			break
		}
	}
	if m := nextWithin(t, a, 10*time.Second); m["t"] != "authRes" {
		t.Fatalf("expected authRes, got %v", m)
	}
}

// A detached run reports stale_seconds once, when its result is applied,
// not when it detaches.
func TestDetachedRunReportsStaleOnApply(t *testing.T) {
	log := &metricLog{}
	e := setupFull(t, func(c *Config) {
		fastDetach(c)
		c.Metrics = log.hook
	}, slowQ)
	a, _ := e.dial(t, nil)
	settleListen(t, e, a)
	subSlowQ(t, a, 1, 1500, 0)
	n0, _ := log.count(MetricStale, "slowq")
	insertName(t, e, "w")
	time.Sleep(800 * time.Millisecond) // detached, not finished
	if n, _ := log.count(MetricStale, "slowq"); n != n0 {
		t.Fatalf("stale_seconds reported while the run was in flight")
	}
	if names := rowNames(nextWithin(t, a, 10*time.Second), 1); len(names) != 1 {
		t.Fatalf("detached result %v", names)
	}
	if n, v := log.count(MetricStale, "slowq"); n != n0+1 || v < 1.2 {
		t.Fatalf("stale_seconds: %d reports (want %d), last %.2fs (want over the 1.5s run)", n-n0, 1, v)
	}
}

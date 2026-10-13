package cometgo

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

// sleepyTable registers "sleepy", whose query sleeps 200 ms, on a table of
// its own, and the method "touch", which inserts a row into that table. Each
// K is its own observer. It returns the table name through *table.
func sleepyTable(t *testing.T, table *string) func(*testEnv, *App) {
	return func(env *testEnv, app *App) {
		*table = "sleepy_" + newID(4)
		ctx := context.Background()
		if _, err := env.pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (id bigserial PRIMARY KEY, name text NOT NULL)`, *table)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { env.pool.Exec(context.Background(), "DROP TABLE "+*table) })
		if err := InstallTriggers(ctx, env.pool, *table); err != nil {
			t.Fatal(err)
		}
		Publish(app, "sleepy", PubOptions{Tables: []string{*table}, Scope: ScopeShared},
			func(c *SubCtx, _ struct{ K int }) (Query, error) {
				return Query{SQL: "SELECT id, name FROM " + *table + ", pg_sleep(0.2) ORDER BY id"}, nil
			})
		Method(app, "touch", func(c *Call, _ struct{}) (struct{}, error) {
			_, err := c.Tx.Exec(c.Context, "INSERT INTO "+*table+" (name) VALUES ('t')")
			return struct{}{}, err
		})
	}
}

// subscribeSleepy subscribes subs 1..n to sleepy and waits for every
// snapshot, over as many ticks as the budget needs.
func subscribeSleepy(t *testing.T, a *client, n int) {
	t.Helper()
	for s := 1; s <= n; s++ {
		a.send(map[string]any{"t": "sub", "s": s, "name": "sleepy", "args": map[string]any{"K": s}})
	}
	ready := map[float64]bool{}
	for len(ready) < n {
		m := nextWithin(t, a, 15*time.Second)
		for _, en := range entries(m) {
			if en["ready"] == true {
				ready[en["s"].(float64)] = true
			}
		}
	}
}

// nextWithin is client.next with a longer timeout, for ticks the budget
// splits.
func nextWithin(t *testing.T, a *client, d time.Duration) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	for {
		_, b, err := a.ws.Read(ctx)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		m := map[string]any{}
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if m["t"] != "pong" {
			return m
		}
	}
}

// With 20 observers of about 200 ms each and a 1 s budget, no tick runs
// much over the budget plus one query.
func TestTickBudgetBoundsTick(t *testing.T) {
	var table string
	log := &metricLog{}
	e := setupFull(t, func(c *Config) {
		c.SlowObserverAfter = 500 * time.Millisecond
		c.Metrics = log.hook
	}, sleepyTable(t, &table))
	a, _ := e.dial(t, nil)
	subscribeSleepy(t, a, 20)
	log.mu.Lock()
	log.seen = nil
	log.mu.Unlock()

	if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+table+" (name) VALUES ('x')"); err != nil {
		t.Fatal(err)
	}
	updated := map[float64]bool{}
	for len(updated) < 20 {
		for _, en := range entries(nextWithin(t, a, 15*time.Second)) {
			if en["ops"] != nil {
				updated[en["s"].(float64)] = true
			}
		}
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	var longest float64
	for _, m := range log.seen[MetricTick] {
		longest = max(longest, m.Value)
	}
	t.Logf("%d ticks, longest %.2fs", len(log.seen[MetricTick]), longest)
	if longest > 1.4 {
		t.Fatalf("a tick took %.2fs; budget 1s plus one 200ms query", longest)
	}
}

// Under constant writes every observer still runs: the ones a budget left
// unstarted go first in the next tick.
func TestTickBudgetNoStarvation(t *testing.T) {
	var table string
	e := setupFull(t, func(c *Config) { c.SlowObserverAfter = 500 * time.Millisecond }, sleepyTable(t, &table))
	a, _ := e.dial(t, nil)
	subscribeSleepy(t, a, 20)

	ctx, stop := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		for ctx.Err() == nil {
			e.pool.Exec(ctx, "INSERT INTO "+table+" (name) VALUES ('w')")
			time.Sleep(100 * time.Millisecond)
		}
	})
	defer wg.Wait()
	defer stop()
	updated := map[float64]bool{}
	deadline := time.Now().Add(8 * time.Second)
	for len(updated) < 20 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of 20 observers ran in 8s of constant writes", len(updated))
		}
		for _, en := range entries(nextWithin(t, a, 15*time.Second)) {
			if en["ops"] != nil {
				updated[en["s"].(float64)] = true
			}
		}
	}
}

// A connection whose observers the budget deferred gets its res only once
// they have all run; another connection's call is not held by them.
func TestTickBudgetHoldsOnlyItsConnection(t *testing.T) {
	var table string
	e := setupFull(t, func(c *Config) {
		c.SlowObserverAfter = 250 * time.Millisecond
		// Long enough that both calls below are drained by one tick.
		c.MinTickInterval = 300 * time.Millisecond
	}, sleepyTable(t, &table))
	e.waitListening(t)
	a, _ := e.dial(t, nil)
	subscribeSleepy(t, a, 10)
	b, _ := e.dial(t, nil)
	subscribe(t, b, 1, "items.all")

	// An empty tick now, so the engine sleeps MinTickInterval while both
	// calls commit, b's first: one tick drains them, b's change older.
	e.app.engine.signal()
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	b.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "x", "Name": "n"}})
	time.Sleep(20 * time.Millisecond)
	a.send(map[string]any{"t": "call", "id": 1, "m": "touch", "args": map[string]any{}})

	if m := b.next(); m["t"] != "tx" {
		t.Fatalf("b: expected its tx, got %v", m)
	}
	if r := b.next(); r["t"] != "res" || r["lag"] != nil {
		t.Fatalf("b: res %v", r)
	}
	if took := time.Since(start); took > 1300*time.Millisecond {
		t.Fatalf("b's res took %s, held behind a's deferred observers", took)
	}

	updated := map[float64]bool{}
	for {
		m := nextWithin(t, a, 15*time.Second)
		if m["t"] == "res" {
			if m["lag"] != nil {
				t.Fatalf("a: res %v", m)
			}
			break
		}
		for _, en := range entries(m) {
			if en["ops"] != nil {
				updated[en["s"].(float64)] = true
			}
		}
	}
	if len(updated) != 10 {
		t.Fatalf("a's res came after only %d of its 10 observers ran", len(updated))
	}
	if took := time.Since(start); took < 800*time.Millisecond {
		t.Fatalf("a's 10 x 200ms observers all ran in %s: the budget did not split them", took)
	}
}

// An observer that stops being due before it runs (its awaiting sub left)
// drops its place in line, so a later change does not put it ahead of
// observers that waited longer.
func TestDueFromClearedWhenNoLongerDue(t *testing.T) {
	e := fenceEngine()
	sub := &subscription{s: 1}
	obs := &observer{key: "k", pub: &publication{name: "p"}, res: &result{}, subs: map[*subscription]struct{}{sub: {}}, dueFrom: 5}
	sub.obs = obs
	e.observers[obs.key] = obs
	e.tick(context.Background())
	if obs.dueFrom != 0 {
		t.Fatalf("dueFrom = %d after a tick where it was not due", obs.dueFrom)
	}
}

package cometgo

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Fault-injection tests. Each injects one fault while clients write and
// watch, then checks that every watcher converges on the table and that no
// call waited anywhere near FenceTimeout. CI runs them with
// go test -race -count=3 -run TestChaos.

// chaosCallBudget is how long one call may take under a fault: well under
// the 5s FenceTimeout, so a call released by FenceTimeout fails the test.
const chaosCallBudget = 3 * time.Second

// readMsg reads the next non-pong message. It reports errors instead of
// failing the test, for use off the test goroutine.
func readMsg(ctx context.Context, ws *websocket.Conn) (map[string]any, error) {
	for {
		_, b, err := ws.Read(ctx)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, err
		}
		if m["t"] != "pong" {
			return m, nil
		}
	}
}

// watcher mirrors sub 1 (items.all) of one client. A goroutine reads the
// socket into msgs; converge applies them on the test goroutine.
type watcher struct {
	m    *mirror
	msgs chan map[string]any
	err  error // why msgs was closed; read only after it is
}

// watch dials n clients subscribed to items.all.
func (e *testEnv) watch(t *testing.T, n int) []*watcher {
	t.Helper()
	ws := make([]*watcher, n)
	for i := range ws {
		c, _ := e.dial(t, nil)
		c.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
		m := &mirror{}
		m.apply(t, c.next())
		ws[i] = follow(c, m)
	}
	return ws
}

// follow starts reading c into a watcher of mirror m.
func follow(c *client, m *mirror) *watcher {
	w := &watcher{m: m, msgs: make(chan map[string]any, 1<<14)}
	go func() {
		defer close(w.msgs)
		for {
			msg, err := readMsg(context.Background(), c.ws)
			if err != nil {
				w.err = err
				return
			}
			w.msgs <- msg
		}
	}()
	return w
}

// converge applies messages until the mirror matches the table, which must
// no longer change, or fails after within.
func (w *watcher) converge(t *testing.T, e *testEnv, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		diff := w.m.matchesDB(t, e)
		if diff == nil {
			return
		}
		select {
		case msg, ok := <-w.msgs:
			if !ok {
				t.Fatalf("watcher socket closed: %v", w.err)
			}
			w.m.apply(t, msg)
		case <-deadline:
			t.Fatalf("watcher did not converge in %v: %v", within, diff)
		}
	}
}

// callLog collects the outcome of the writers' calls.
type callLog struct {
	mu       sync.Mutex
	ok, errs int
	problems []string // stalled, lagged or lost calls
}

func (l *callLog) add(ok bool, problem string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ok {
		l.ok++
	} else {
		l.errs++
	}
	if problem != "" {
		l.problems = append(l.problems, problem)
	}
}

// writers starts n clients that each add a row and then bump it in a loop,
// until the returned stop is called. stop waits for them and returns their
// log. A call may fail (the database may be down) but must answer within
// chaosCallBudget, and a call that succeeded must not report lag.
func (e *testEnv) writers(t *testing.T, n int) (stop func() *callLog) {
	t.Helper()
	log := &callLog{}
	done := make(chan struct{})
	var wg sync.WaitGroup
	for w := range n {
		c, _ := e.dial(t, nil)
		wg.Go(func() {
			var row string
			for i := 1; ; i++ {
				select {
				case <-done:
					return
				default:
				}
				call := map[string]any{"t": "call", "id": i, "m": "add", "args": map[string]any{"Owner": "w", "Name": fmt.Sprintf("w%d", w)}}
				if row != "" {
					call["m"], call["args"] = "bump", map[string]any{"ID": row}
				}
				b, _ := json.Marshal(call)
				start := time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), e.app.cfg.FenceTimeout+2*time.Second)
				err := c.ws.Write(ctx, websocket.MessageText, b)
				var res map[string]any
				for err == nil && (res["t"] != "res" || res["id"] != float64(i)) {
					res, err = readMsg(ctx, c.ws)
				}
				cancel()
				if err != nil {
					log.add(false, fmt.Sprintf("writer %d call %d: %v", w, i, err))
					return
				}
				problem := ""
				if d := time.Since(start); d > chaosCallBudget {
					problem = fmt.Sprintf("writer %d call %d took %v: %v", w, i, d, res)
				} else if res["lag"] == true {
					problem = fmt.Sprintf("writer %d call %d lagged: %v", w, i, res)
				}
				log.add(res["ok"] == true, problem)
				if res["ok"] == true && row == "" {
					row = res["val"].(string)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
	return func() *callLog {
		close(done)
		wg.Wait()
		return log
	}
}

// check fails the test if any call stalled or lagged, or none succeeded.
func (l *callLog) check(t *testing.T) {
	t.Helper()
	t.Logf("calls: %d ok, %d failed", l.ok, l.errs)
	for _, p := range l.problems {
		t.Error(p)
	}
	if l.ok == 0 {
		t.Error("no call succeeded")
	}
}

// waitDB waits until the test pool can query again.
func (e *testEnv) waitDB(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if _, err := e.pool.Exec(context.Background(), "SELECT 1"); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("database did not come back: %v", err)
		}
	}
}

// The listener's connection is killed mid-traffic, several times in a
// row. Every call still answers well within FenceTimeout, without lag.
func TestChaosListenerKilled(t *testing.T) {
	appName := "cometgo_chaos_" + newID(4)
	e := setupWith(t, withAppName(t, appName))
	ws := e.watch(t, 3)
	e.waitListening(t)
	stop := e.writers(t, 4)

	ctx := context.Background()
	for range 5 {
		time.Sleep(200 * time.Millisecond)
		before := e.listened()
		var pid int32
		err := e.pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity
			WHERE application_name = $1 AND query LIKE 'LISTEN %'`, appName).Scan(&pid)
		if err != nil {
			stop()
			t.Fatalf("find listener: %v", err)
		}
		if _, err := e.pool.Exec(ctx, "SELECT pg_terminate_backend($1)", pid); err != nil {
			stop()
			t.Fatal(err)
		}
		for deadline := time.Now().Add(chaosCallBudget); e.listened() == before; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				stop()
				t.Fatalf("listener did not reconnect within %v", chaosCallBudget)
			}
		}
	}
	time.Sleep(200 * time.Millisecond)
	stop().check(t)
	for _, w := range ws {
		w.converge(t, e, 5*time.Second)
	}
}

// Postgres restarts while clients are connected and writing. Calls made
// while it is down fail fast; once it is back, calls succeed and every
// watcher converges.
//
// It needs COMETGO_TEST_PG_RESTART, a shell command that restarts the test
// database, e.g. `docker compose restart postgres`.
func TestChaosPostgresRestart(t *testing.T) {
	restart := os.Getenv("COMETGO_TEST_PG_RESTART")
	if restart == "" {
		t.Skip("COMETGO_TEST_PG_RESTART not set")
	}
	e := setup(t)
	ws := e.watch(t, 3)
	e.waitListening(t)
	stop := e.writers(t, 4)

	time.Sleep(300 * time.Millisecond)
	before := e.listened()
	if out, err := exec.Command("sh", "-c", restart).CombinedOutput(); err != nil {
		stop()
		t.Fatalf("restart: %v: %s", err, out)
	}
	e.waitDB(t)
	for deadline := time.Now().Add(15 * time.Second); e.listened() == before; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			stop()
			t.Fatal("listener did not reconnect after the restart")
		}
	}
	// Traffic on the recovered database.
	time.Sleep(time.Second)
	stop().check(t)
	for _, w := range ws {
		w.converge(t, e, 10*time.Second)
	}
}

// A publication fn that panics, subscribed to over and over while others
// write and watch, ends only its own subs.
func TestChaosPublicationPanics(t *testing.T) {
	e := setupFull(t, nil, func(env *testEnv, app *App) {
		Publish(app, "boom", PubOptions{Tables: []string{env.table}}, func(c *SubCtx, _ struct{}) (Query, error) {
			panic("boom in fn")
		})
	})
	ws := e.watch(t, 3)
	stop := e.writers(t, 4)

	p, _ := e.dial(t, nil)
	for s := 1; s <= 50; s++ {
		p.send(map[string]any{"t": "sub", "s": s, "name": "boom"})
		en := entries(p.next())
		if end, _ := en[0]["end"].(map[string]any); en[0]["s"] != float64(s) || !isInternal(end) {
			stop()
			t.Fatalf("boom sub %d: %v", s, en)
		}
	}
	stop().check(t)
	for _, w := range ws {
		w.converge(t, e, 5*time.Second)
	}
}

// An observer whose query takes 10s stalls the engine (isolating it is
// M3), but nothing crashes and every watcher converges once it is gone.
// Calls are not held to chaosCallBudget here.
func TestChaosSlowObserverQuery(t *testing.T) {
	if testing.Short() {
		t.Skip("takes 10s")
	}
	e := setupFull(t, nil, func(env *testEnv, app *App) {
		Publish(app, "slow", PubOptions{Tables: []string{env.table}}, func(c *SubCtx, _ struct{}) (Query, error) {
			return Query{SQL: "SELECT 1 AS id FROM pg_sleep(10)"}, nil
		})
	})
	ws := e.watch(t, 3)
	stop := e.writers(t, 2)

	s, _ := e.dial(t, nil)
	s.send(map[string]any{"t": "sub", "s": 1, "name": "slow"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	msg, err := readMsg(ctx, s.ws)
	if err != nil {
		stop()
		t.Fatalf("slow snapshot: %v", err)
	}
	if en := entries(msg); en[0]["s"] != float64(1) || en[0]["ready"] != true {
		stop()
		t.Fatalf("slow snapshot %v", en)
	}
	s.send(map[string]any{"t": "unsub", "s": 1})
	log := stop()
	t.Logf("calls: %d ok, %d failed", log.ok, log.errs)
	for _, w := range ws {
		w.converge(t, e, 15*time.Second)
	}
	// With the slow observer gone, calls are fast again.
	a, _ := e.dial(t, nil)
	start := time.Now()
	a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "a", "Name": "after"}})
	if res := a.next(); res["t"] != "res" || res["ok"] != true || res["lag"] == true {
		t.Fatalf("res %v", res)
	}
	if d := time.Since(start); d > chaosCallBudget {
		t.Fatalf("call after slow observer took %v", d)
	}
}

// A client that stops reading while others write coalesces, then drains
// and converges; the other clients' calls are not held up meanwhile.
func TestChaosSlowClientCoalescesThenDrains(t *testing.T) {
	e := setup(t)
	a, c, m := stalledClient(t, e)
	ws := e.watch(t, 2)
	stop := e.writers(t, 2)

	for i := 0; i < 300 && !c.coalescing.Load(); i++ {
		e.rewrite(t, i)
		time.Sleep(10 * time.Millisecond)
	}
	coalesced := c.coalescing.Load()
	stop().check(t)
	if !coalesced {
		t.Fatal("connection never coalesced")
	}
	follow(a, m).converge(t, e, 10*time.Second)
	if !m.reset {
		t.Fatal("drained without a reset")
	}
	for _, w := range ws {
		w.converge(t, e, 5*time.Second)
	}
	for deadline := time.Now().Add(2 * time.Second); c.coalescing.Load(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("still coalescing after drain")
		}
	}
}

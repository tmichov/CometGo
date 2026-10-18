package cometgo

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// errLogs records the err attr of every logged record.
type errLogs struct {
	mu   sync.Mutex
	errs []string
}

func (l *errLogs) Enabled(context.Context, slog.Level) bool { return true }
func (l *errLogs) Handle(_ context.Context, r slog.Record) error {
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "err" {
			l.mu.Lock()
			l.errs = append(l.errs, a.Value.String())
			l.mu.Unlock()
		}
		return true
	})
	return nil
}
func (l *errLogs) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *errLogs) WithGroup(string) slog.Handler      { return l }

// A panicking publication fn ends only its own sub, with INTERNAL, and the
// panic is logged with its stack.
func TestPublicationPanicEndsOnlyThatSub(t *testing.T) {
	logs := &errLogs{}
	e := setupFull(t, func(c *Config) { c.Logger = slog.New(logs) }, func(env *testEnv, app *App) {
		Publish(app, "boom", PubOptions{Tables: []string{env.table}}, func(c *SubCtx, _ struct{}) (Query, error) {
			// Only alice gets a query, so a rebind away from alice panics.
			if c.Identity == nil || c.Identity.UserID != "alice" {
				panic("boom in fn")
			}
			return Query{SQL: "SELECT id FROM " + env.table + " ORDER BY id"}, nil
		})
	})
	a, _ := e.dial(t, nil)
	b, _ := e.dial(t, nil)
	for _, c := range []*client{a, b} {
		c.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
		if en := entries(c.next()); en[0]["s"] != float64(1) || en[0]["ready"] != true {
			t.Fatalf("snapshot %v", en)
		}
	}

	a.send(map[string]any{"t": "sub", "s": 2, "name": "boom"})
	en := entries(a.next())
	end, _ := en[0]["end"].(map[string]any)
	if en[0]["s"] != float64(2) || !isInternal(end) {
		t.Fatalf("got %v", en)
	}
	logs.mustHavePanic(t)

	// Both connections' items.all subs still receive tx.
	a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "x", "Name": "after"}})
	for _, c := range []*client{a, b} {
		tx := c.next()
		if tx["t"] != "tx" || entries(tx)[0]["s"] != float64(1) {
			t.Fatalf("expected tx for s1, got %v", tx)
		}
	}
	if r := a.next(); r["t"] != "res" || r["ok"] != true {
		t.Fatalf("res %v", r)
	}
}

// On an identity rebind, a sub whose fn panics for the new identity ends with
// INTERNAL; the connection's other subs are rebound and stay live.
func TestPublicationPanicOnRebindEndsOnlyThatSub(t *testing.T) {
	logs := &errLogs{}
	e := setupFull(t, func(c *Config) { c.Logger = slog.New(logs) }, func(env *testEnv, app *App) {
		Publish(app, "boom", PubOptions{Tables: []string{env.table}}, func(c *SubCtx, _ struct{}) (Query, error) {
			if c.Identity == nil || c.Identity.UserID != "alice" {
				panic("boom in fn")
			}
			return Query{SQL: "SELECT id FROM " + env.table + " ORDER BY id"}, nil
		})
	})
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.mine"})
	a.next()
	a.send(map[string]any{"t": "sub", "s": 2, "name": "boom"})
	if en := entries(a.next()); en[0]["s"] != float64(2) || en[0]["ready"] != true {
		t.Fatalf("alice boom snapshot %v", en)
	}

	a.send(map[string]any{"t": "auth", "id": 1, "token": "bob"})
	got := map[float64]map[string]any{}
	for {
		m := a.next()
		if m["t"] == "authRes" {
			if m["ok"] != true {
				t.Fatalf("authRes %v", m)
			}
			break
		}
		for _, en := range entries(m) {
			got[en["s"].(float64)] = en
		}
	}
	if end, _ := got[2]["end"].(map[string]any); !isInternal(end) {
		t.Fatalf("boom sub after rebind: %v", got[2])
	}
	if got[1]["reset"] != true || got[1]["end"] != nil {
		t.Fatalf("items.mine after rebind: %v", got[1])
	}
	logs.mustHavePanic(t)

	// items.mine is live for bob.
	a.send(map[string]any{"t": "call", "id": 2, "m": "add", "args": map[string]any{"Owner": "bob", "Name": "b1"}})
	if tx := a.next(); tx["t"] != "tx" || entries(tx)[0]["s"] != float64(1) {
		t.Fatalf("expected tx for s1, got %v", tx)
	}
}

// panicValuer is a query arg whose Value dereferences nil once armed.
type panicValuer struct{ armed *atomic.Bool }

func (v panicValuer) Value() (driver.Value, error) {
	if v.armed.Load() {
		var p *int64
		return *p, nil
	}
	return int64(1), nil
}

// A Query.Args value that panics while the observer re-runs ends that
// observer's sub with INTERNAL; another observer re-run in the same tick
// still sends its tx.
func TestQueryArgsPanicEndsOnlyThatObserver(t *testing.T) {
	logs := &errLogs{}
	var armed atomic.Bool
	e := setupFull(t, func(c *Config) { c.Logger = slog.New(logs) }, func(env *testEnv, app *App) {
		Publish(app, "boomArgs", PubOptions{Tables: []string{env.table}}, func(c *SubCtx, _ struct{}) (Query, error) {
			return Query{SQL: "SELECT id FROM " + env.table + " WHERE $1::int8 = 1 ORDER BY id", Args: []any{panicValuer{&armed}}}, nil
		})
	})
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	a.next()
	a.send(map[string]any{"t": "sub", "s": 2, "name": "boomArgs"})
	if en := entries(a.next()); en[0]["s"] != float64(2) || en[0]["ready"] != true {
		t.Fatalf("boomArgs snapshot %v", en)
	}

	// Both observers read the table, so this write re-runs both in one tick.
	armed.Store(true)
	a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "x", "Name": "after"}})
	got := map[float64]map[string]any{}
	for {
		m := a.next()
		if m["t"] == "res" {
			if m["ok"] != true {
				t.Fatalf("res %v", m)
			}
			break
		}
		for _, en := range entries(m) {
			got[en["s"].(float64)] = en
		}
	}
	if end, _ := got[2]["end"].(map[string]any); !isInternal(end) {
		t.Fatalf("boomArgs sub: %v", got[2])
	}
	if ops, _ := got[1]["ops"].([]any); got[1]["end"] != nil || len(ops) != 1 {
		t.Fatalf("items.all sub: %v", got[1])
	}
	logs.mu.Lock()
	logged := strings.Join(logs.errs, "\n")
	logs.mu.Unlock()
	if !strings.Contains(logged, "panic: runtime error: invalid memory address") || !strings.Contains(logged, "runtime/debug.Stack") {
		t.Fatalf("panic not logged with stack: %q", logged)
	}

	// The engine keeps running: items.all still gets live diffs.
	a.send(map[string]any{"t": "call", "id": 2, "m": "add", "args": map[string]any{"Owner": "x", "Name": "later"}})
	if tx := a.next(); tx["t"] != "tx" || entries(tx)[0]["s"] != float64(1) {
		t.Fatalf("expected tx for s1, got %v", tx)
	}
}

// isInternal reports whether end is a generic INTERNAL error that carries a
// ref and leaks nothing of the panic.
func isInternal(end map[string]any) bool {
	b, _ := json.Marshal(end)
	return end["code"] == CodeInternal && end["msg"] == "internal error" &&
		!strings.Contains(string(b), "boom") && !strings.Contains(string(b), "goroutine")
}

func (l *errLogs) mustHavePanic(t *testing.T) {
	t.Helper()
	l.mu.Lock()
	logged := strings.Join(l.errs, "\n")
	l.mu.Unlock()
	if !strings.Contains(logged, "panic: boom in fn") || !strings.Contains(logged, "runtime/debug.Stack") {
		t.Fatalf("panic not logged with stack: %q", logged)
	}
}

// panicTracer panics once, on the first statement starting with prefix,
// after being armed.
type panicTracer struct {
	prefix string
	armed  atomic.Bool
}

func (p *panicTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(d.SQL, p.prefix) && p.armed.CompareAndSwap(true, false) {
		panic("boom in tracer")
	}
	return ctx
}

// tracedPool gives the app its own pool with tr as its tracer and logs to
// logs.
func tracedPool(t *testing.T, tr *panicTracer, logs *errLogs) func(*Config) {
	return func(c *Config) {
		c.Logger = slog.New(logs)
		cfg, err := pgxpool.ParseConfig(os.Getenv("COMETGO_TEST_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.ConnConfig.Tracer = tr
		pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		c.DB = pool
	}
}

// mustHaveTracerPanic fails unless the tracer's panic was logged with its
// stack.
func (l *errLogs) mustHaveTracerPanic(t *testing.T) {
	t.Helper()
	l.mu.Lock()
	logged := strings.Join(l.errs, "\n")
	l.mu.Unlock()
	if !strings.Contains(logged, "panic: boom in tracer") || !strings.Contains(logged, "runtime/debug.Stack") {
		t.Fatalf("panic not logged with stack: %q", logged)
	}
}

func (p *panicTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A pool tracer that panics while the engine opens a tick's snapshot does
// not crash the server: the tick fails, is retried, and the write still
// reaches the sub before the call's res. A panic during BEGIN must not leak
// the pool connection either, or the test's pool.Close hangs.
func TestPoolTracerPanicRetriesTick(t *testing.T) {
	// The snapshot's BEGIN (before a tx holds the connection) and its SET
	// LOCAL (inside the tx).
	for _, prefix := range []string{"begin isolation level repeatable read", "SET LOCAL statement_timeout"} {
		t.Run(prefix, func(t *testing.T) {
			logs := &errLogs{}
			tr := &panicTracer{prefix: prefix}
			e := setupFull(t, tracedPool(t, tr, logs), nil)
			a, _ := e.dial(t, nil)
			subscribe(t, a, 1, "items.all")

			tr.armed.Store(true)
			a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "x", "Name": "after"}})
			if tx := a.next(); tx["t"] != "tx" || entries(tx)[0]["s"] != float64(1) {
				t.Fatalf("expected tx for s1, got %v", tx)
			}
			if r := a.next(); r["t"] != "res" || r["ok"] != true {
				t.Fatalf("res %v", r)
			}
			if tr.armed.Load() {
				t.Fatal("tracer never panicked")
			}
			logs.mustHaveTracerPanic(t)
		})
	}
}

// A pool tracer that panics on a method call's BEGIN or COMMIT fails only
// that call, with INTERNAL, and drops any fence it registered. The server
// keeps answering calls. No sub is open, so the engine runs no BEGIN or
// COMMIT of its own that the tracer could hit instead.
func TestPoolTracerPanicFailsOnlyThatCall(t *testing.T) {
	for _, sql := range []string{"begin", "commit"} {
		t.Run(sql, func(t *testing.T) {
			logs := &errLogs{}
			tr := &panicTracer{prefix: sql}
			e := setupFull(t, tracedPool(t, tr, logs), nil)
			a, _ := e.dial(t, nil)
			tr.armed.Store(true)
			a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "x", "Name": "boom-" + sql}})
			r := a.next()
			if end, _ := r["err"].(map[string]any); r["t"] != "res" || r["ok"] != false || !isInternal(end) {
				t.Fatalf("res %v", r)
			}
			if tr.armed.Load() {
				t.Fatal("tracer never panicked")
			}
			logs.mustHaveTracerPanic(t)
			e.app.engine.mu.Lock()
			fences := len(e.app.engine.fences)
			e.app.engine.mu.Unlock()
			if fences != 0 {
				t.Fatalf("%d fences left registered", fences)
			}
			if n := e.countRows(t, "SELECT count(*) FROM "+e.table+" WHERE name = 'boom-"+sql+"'"); n != 0 {
				t.Fatalf("failed call wrote %d rows", n)
			}
			a.send(map[string]any{"t": "call", "id": 2, "m": "add", "args": map[string]any{"Owner": "x", "Name": "after"}})
			if r := a.next(); r["t"] != "res" || r["ok"] != true {
				t.Fatalf("call after the panic: %v", r)
			}
		})
	}
}

// A pool tracer that panics on the listener's LISTEN drops that
// connection like a network error: the listener reconnects and live
// updates flow.
func TestPoolTracerPanicOnListenReconnects(t *testing.T) {
	skipLogical(t, "a panic on the LISTEN connection")
	logs := &errLogs{}
	tr := &panicTracer{prefix: "LISTEN "}
	tr.armed.Store(true) // the first LISTEN, at Start
	e := setupFull(t, tracedPool(t, tr, logs), nil)
	e.waitListening(t)
	if tr.armed.Load() {
		t.Fatal("tracer never panicked")
	}
	logs.mustHaveTracerPanic(t)
	a, _ := e.dial(t, nil)
	subscribe(t, a, 1, "items.all")
	if _, err := e.pool.Exec(context.Background(), "INSERT INTO "+e.table+" (owner, name) VALUES ('x', 'live')"); err != nil {
		t.Fatal(err)
	}
	if tx := a.next(); tx["t"] != "tx" || entries(tx)[0]["s"] != float64(1) {
		t.Fatalf("expected tx for s1, got %v", tx)
	}
}

// Describe returns an error, rather than crashing codegen, when encoding a
// publication's Query.Args panics.
func TestDescribeArgsPanicReturnsError(t *testing.T) {
	var armed atomic.Bool
	armed.Store(true)
	e := setupFull(t, nil, func(env *testEnv, app *App) {
		Publish(app, "boomArgs", PubOptions{Tables: []string{env.table}}, func(c *SubCtx, _ struct{}) (Query, error) {
			return Query{SQL: "SELECT id FROM " + env.table + " WHERE $1::int8 = 1", Args: []any{panicValuer{&armed}}}, nil
		})
	})
	_, err := Describe(context.Background(), e.app, DescribeOptions{Identity: &Identity{UserID: "u"}})
	if err == nil || !strings.Contains(err.Error(), "boomArgs") || !strings.Contains(err.Error(), "panic: runtime error") {
		t.Fatalf("got %v", err)
	}
}

// errValuer panics with err when its Value is called.
type errValuer struct{ err error }

func (v errValuer) Value() (driver.Value, error) { panic(v.err) }

// A panic carrying an error keeps it classifiable: a panicked errTooLarge
// ends the sub with RESULT_TOO_LARGE, not INTERNAL. A panicked *Error is
// still INTERNAL: a panic never chooses the client's error.
func TestPanickedErrorKeepsItsClass(t *testing.T) {
	e := setupFull(t, nil, func(env *testEnv, app *App) {
		for name, err := range map[string]error{"tooLarge": errTooLarge, "forbidden": ErrForbidden} {
			Publish(app, name, PubOptions{Tables: []string{env.table}}, func(c *SubCtx, _ struct{}) (Query, error) {
				return Query{SQL: "SELECT id FROM " + env.table + " WHERE $1::int8 = 1", Args: []any{errValuer{err}}}, nil
			})
		}
	})
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "tooLarge"})
	if end, _ := entries(a.next())[0]["end"].(map[string]any); end["code"] != CodeResultTooLarge {
		t.Fatalf("tooLarge end: %v", end)
	}
	a.send(map[string]any{"t": "sub", "s": 2, "name": "forbidden"})
	if end, _ := entries(a.next())[0]["end"].(map[string]any); end["code"] != CodeInternal {
		t.Fatalf("forbidden end: %v", end)
	}
}

// A call waiting on its fence does not hold a pool connection: with the
// engine stuck in a slow query, the only connection acquired is the
// engine's.
func TestFenceWaitReleasesConnection(t *testing.T) {
	e := setupFull(t, nil, func(env *testEnv, app *App) {
		Publish(app, "slow", PubOptions{Tables: []string{env.table}}, func(c *SubCtx, _ struct{}) (Query, error) {
			return Query{SQL: "SELECT 1 AS id FROM pg_sleep(3)"}, nil
		})
	})
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "slow"})
	a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "x", "Name": "fenced"}})
	// The call has committed once its row is visible; its fence waits for
	// the stalled engine.
	for deadline := time.Now().Add(2 * time.Second); e.countRows(t, "SELECT count(*) FROM "+e.table+" WHERE name = 'fenced'") == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("call never committed")
		}
	}
	// Within the slow query's 3s, only the engine's connection stays out.
	for deadline := time.Now().Add(time.Second); e.pool.Stat().AcquiredConns() != 1; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d connections acquired during the fence wait, want 1 (the engine's)", e.pool.Stat().AcquiredConns())
		}
	}
}

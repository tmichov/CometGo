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

// panicTracer panics once, in the engine's SET LOCAL, after being armed.
type panicTracer struct{ armed atomic.Bool }

func (p *panicTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(d.SQL, "SET LOCAL statement_timeout") && p.armed.CompareAndSwap(true, false) {
		panic("boom in tracer")
	}
	return ctx
}

func (p *panicTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A pool tracer that panics while the engine opens a tick's snapshot does
// not crash the server: the tick fails, is retried, and the write still
// reaches the sub before the call's res.
func TestPoolTracerPanicRetriesTick(t *testing.T) {
	logs := &errLogs{}
	tr := &panicTracer{}
	e := setupFull(t, func(c *Config) {
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
	}, nil)
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
	logs.mu.Lock()
	logged := strings.Join(logs.errs, "\n")
	logs.mu.Unlock()
	if !strings.Contains(logged, "panic: boom in tracer") || !strings.Contains(logged, "runtime/debug.Stack") {
		t.Fatalf("panic not logged with stack: %q", logged)
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

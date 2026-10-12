package cometgo

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
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

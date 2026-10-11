package cometgo

import (
	"context"
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
		Publish(app, "boom", PubOptions{Tables: []string{env.table}}, func(*SubCtx, struct{}) (Query, error) {
			panic("boom in fn")
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
	if en[0]["s"] != float64(2) || en[0]["end"].(map[string]any)["code"] != CodeInternal {
		t.Fatalf("got %v", en)
	}
	logs.mu.Lock()
	logged := strings.Join(logs.errs, "\n")
	logs.mu.Unlock()
	if !strings.Contains(logged, "panic: boom in fn") || !strings.Contains(logged, "runtime/debug.Stack") {
		t.Fatalf("panic not logged with stack: %q", logged)
	}

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

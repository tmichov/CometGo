package cometgo

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// members is the access list the "team" publication's fn checks.
type members struct {
	mu sync.Mutex
	in map[string]bool
}

func (m *members) has(u string) bool { m.mu.Lock(); defer m.mu.Unlock(); return m.in[u] }
func (m *members) revoke(u string)   { m.mu.Lock(); defer m.mu.Unlock(); delete(m.in, u) }

// setupTeam registers "team", a ScopeShared publication whose fn admits
// only members, a check its query does not read.
func setupTeam(t *testing.T) (*testEnv, *members) {
	m := &members{in: map[string]bool{"alice": true, "bob": true}}
	e := setupFull(t, nil, func(env *testEnv, app *App) {
		Publish(app, "team", PubOptions{Tables: []string{env.table}, Scope: ScopeShared},
			func(c *SubCtx, _ struct{}) (Query, error) {
				if c.Identity == nil || !m.has(c.Identity.UserID) {
					return Query{}, ErrForbidden
				}
				return Query{SQL: "SELECT id, name FROM " + env.table + " ORDER BY id"}, nil
			})
	})
	return e, m
}

func isForbiddenEnd(en map[string]any) bool {
	end, _ := en["end"].(map[string]any)
	return end["code"] == CodeForbidden
}

// After Revalidate a revoked sub ends with FORBIDDEN; the user's other sub
// and other users' subs on the same observer keep streaming.
func TestRevalidateEndsRevokedSubs(t *testing.T) {
	e, m := setupTeam(t)
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	b, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "bob"}})
	a.send(map[string]any{"t": "sub", "s": 1, "name": "team"})
	a.next()
	a.send(map[string]any{"t": "sub", "s": 2, "name": "items.mine"})
	a.next()
	b.send(map[string]any{"t": "sub", "s": 1, "name": "team"})
	b.next()

	m.revoke("alice")
	e.app.Revalidate("alice")
	if en := entries(a.next()); len(en) != 1 || en[0]["s"] != float64(1) || !isForbiddenEnd(en[0]) {
		t.Fatalf("alice team sub: %v", en)
	}

	a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "alice", "Name": "n"}})
	if en := entries(a.next()); len(en) != 1 || en[0]["s"] != float64(2) || en[0]["ops"] == nil {
		t.Fatalf("alice items.mine: %v", en)
	}
	if r := a.next(); r["t"] != "res" || r["ok"] != true {
		t.Fatalf("res %v", r)
	}
	if en := entries(b.next()); en[0]["s"] != float64(1) || en[0]["ops"] == nil {
		t.Fatalf("bob team sub: %v", en)
	}
}

// A session waiting to be resumed is re-checked too; the end reaches the
// client in the replay.
func TestRevalidateReachesDetachedSession(t *testing.T) {
	e, m := setupTeam(t)
	a, w := e.dial(t, resumeHello(map[string]any{"auth": map[string]any{"token": "alice"}}))
	a.send(map[string]any{"t": "sub", "s": 1, "name": "team"})
	a.next()
	a.send(map[string]any{"t": "sub", "s": 2, "name": "items.mine"})
	last := a.next()
	a.kill()
	time.Sleep(50 * time.Millisecond)

	m.revoke("alice")
	e.app.Revalidate("alice")
	time.Sleep(100 * time.Millisecond)

	b, w2 := e.dial(t, resumeHello(map[string]any{
		"auth":   map[string]any{"token": "alice"},
		"resume": map[string]any{"session": w["session"], "seq": seqOf(last), "subs": []int{1, 2}},
	}))
	if w2["resumed"] != true {
		t.Fatalf("welcome %v", w2)
	}
	if en := entries(b.next()); len(en) != 1 || en[0]["s"] != float64(1) || !isForbiddenEnd(en[0]) {
		t.Fatalf("replay: %v", en)
	}
}

// A panic in fn during Revalidate ends that sub with INTERNAL instead of
// taking down the engine.
func TestRevalidateRecoversPanic(t *testing.T) {
	var panicNow atomic.Bool
	e := setupFull(t, nil, func(env *testEnv, app *App) {
		Publish(app, "flaky", PubOptions{Tables: []string{env.table}},
			func(c *SubCtx, _ struct{}) (Query, error) {
				if panicNow.Load() {
					panic("flaky fn")
				}
				return Query{SQL: "SELECT id FROM " + env.table}, nil
			})
	})
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	a.send(map[string]any{"t": "sub", "s": 1, "name": "flaky"})
	a.next()
	panicNow.Store(true)
	e.app.Revalidate("alice")
	en := entries(a.next())
	if end, _ := en[0]["end"].(map[string]any); end["code"] != CodeInternal {
		t.Fatalf("got %v", en)
	}
}

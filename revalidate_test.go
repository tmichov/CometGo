package cometgo

import (
	"context"
	"fmt"
	"net/http/httptest"
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
	e.waitListening(t) // Revalidate reaches the engine through LISTEN
	return e, m
}

// revalidate calls e.app.Revalidate and fails the test on error.
func (e *testEnv) revalidate(t *testing.T, userID string) {
	t.Helper()
	if err := e.app.Revalidate(context.Background(), userID); err != nil {
		t.Fatal(err)
	}
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
	e.revalidate(t, "alice")
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
	e.revalidate(t, "alice")
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
	e.waitListening(t)
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	a.send(map[string]any{"t": "sub", "s": 1, "name": "flaky"})
	a.next()
	panicNow.Store(true)
	e.revalidate(t, "alice")
	en := entries(a.next())
	if end, _ := en[0]["end"].(map[string]any); end["code"] != CodeInternal {
		t.Fatalf("got %v", en)
	}
}

// dbTeam registers "dbteam", a ScopeShared publication whose fn admits only
// users listed in a membership table, which its query does not read, and
// the method "leave", which removes a membership and revalidates after
// commit (and then fails if Fail is set).
func dbTeam(t *testing.T, members *string) func(*testEnv, *App) {
	return func(env *testEnv, app *App) {
		*members = "members_" + newID(4)
		ctx := context.Background()
		if _, err := env.pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (user_id text PRIMARY KEY);
			INSERT INTO %[1]s VALUES ('alice'), ('bob')`, *members)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { env.pool.Exec(context.Background(), "DROP TABLE "+*members) })
		publishDBTeam(app, env, *members)
		Method(app, "leave", func(c *Call, a struct {
			User string
			Fail bool
		}) (struct{}, error) {
			if _, err := c.Tx.Exec(c.Context, "DELETE FROM "+*members+" WHERE user_id = $1", a.User); err != nil {
				return struct{}{}, err
			}
			if err := c.RevalidateAfterCommit(a.User); err != nil {
				return struct{}{}, err
			}
			if a.Fail {
				return struct{}{}, ValidationError(map[string]string{"user": "refused"})
			}
			return struct{}{}, nil
		})
	}
}

func publishDBTeam(app *App, env *testEnv, members string) {
	Publish(app, "dbteam", PubOptions{Tables: []string{env.table}, Scope: ScopeShared},
		func(c *SubCtx, _ struct{}) (Query, error) {
			var ok bool
			if c.Identity != nil {
				if err := env.pool.QueryRow(c.Context, "SELECT EXISTS (SELECT 1 FROM "+members+" WHERE user_id = $1)", c.Identity.UserID).Scan(&ok); err != nil {
					return Query{}, err
				}
			}
			if !ok {
				return Query{}, ErrForbidden
			}
			return Query{SQL: "SELECT id, name FROM " + env.table + " ORDER BY id"}, nil
		})
}

// A method that removes a membership and calls RevalidateAfterCommit ends
// the revoked sub once it commits, ahead of its res. If the method rolls
// back, nothing ends.
func TestRevalidateAfterCommit(t *testing.T) {
	var members string
	e := setupFull(t, nil, dbTeam(t, &members))
	e.waitListening(t)
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	a.send(map[string]any{"t": "sub", "s": 1, "name": "dbteam"})
	if en := entries(a.next()); en[0]["ready"] != true {
		t.Fatalf("snapshot %v", en)
	}

	a.send(map[string]any{"t": "call", "id": 1, "m": "leave", "args": map[string]any{"User": "alice", "Fail": true}})
	if r := a.next(); r["t"] != "res" || r["ok"] != false {
		t.Fatalf("failed leave: %v", r)
	}
	// Still live: the next write reaches it as a diff, not an end.
	a.send(map[string]any{"t": "call", "id": 2, "m": "add", "args": map[string]any{"Owner": "x", "Name": "n"}})
	if en := entries(a.next()); en[0]["s"] != float64(1) || en[0]["ops"] == nil {
		t.Fatalf("after rolled-back leave: %v", en)
	}
	if r := a.next(); r["t"] != "res" || r["ok"] != true {
		t.Fatalf("res %v", r)
	}

	a.send(map[string]any{"t": "call", "id": 3, "m": "leave", "args": map[string]any{"User": "alice"}})
	if en := entries(a.next()); len(en) != 1 || en[0]["s"] != float64(1) || !isForbiddenEnd(en[0]) {
		t.Fatalf("expected the end before res, got %v", en)
	}
	if r := a.next(); r["t"] != "res" || r["ok"] != true {
		t.Fatalf("leave res %v", r)
	}
}

// Two Apps on one database stand for two nodes. Revalidate on one ends the
// revoked sub held on the other; another user's sub there carries on.
func TestRevalidateReachesOtherNode(t *testing.T) {
	var members string
	e := setupFull(t, nil, dbTeam(t, &members))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	other := New(Config{
		DB:              e.pool,
		MinTickInterval: 5 * time.Millisecond,
		Authenticate: func(ctx context.Context, token string) (*Identity, error) {
			return &Identity{UserID: token}, nil
		},
	})
	publishDBTeam(other, e, members)
	other.Start(ctx)
	srv := httptest.NewServer(other)
	t.Cleanup(srv.Close)
	e2 := &testEnv{app: other, srv: srv, pool: e.pool, table: e.table}
	e.waitListening(t)
	e2.waitListening(t)

	a, _ := e2.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	b, _ := e2.dial(t, map[string]any{"auth": map[string]any{"token": "bob"}})
	for _, c := range []*client{a, b} {
		c.send(map[string]any{"t": "sub", "s": 1, "name": "dbteam"})
		if en := entries(c.next()); en[0]["ready"] != true {
			t.Fatalf("snapshot %v", en)
		}
	}

	if _, err := e.pool.Exec(ctx, "DELETE FROM "+members+" WHERE user_id = 'alice'"); err != nil {
		t.Fatal(err)
	}
	e.revalidate(t, "alice")
	if en := entries(a.next()); len(en) != 1 || !isForbiddenEnd(en[0]) {
		t.Fatalf("alice on the other node: %v", en)
	}
	if _, err := e.pool.Exec(ctx, "INSERT INTO "+e.table+" (owner, name) VALUES ('x', 'y')"); err != nil {
		t.Fatal(err)
	}
	if en := entries(b.next()); en[0]["s"] != float64(1) || en[0]["ops"] == nil {
		t.Fatalf("bob on the other node: %v", en)
	}
}

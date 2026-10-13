package cometgo

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
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
	c := e.session(t, w["session"])
	waitConn(t, c, "detached", func() bool { return c.sock == nil })

	m.revoke("alice")
	e.revalidate(t, "alice")
	// The end gets the next seq once the engine has re-checked the session.
	waitConn(t, c, "the end", func() bool { return c.seq > seqOf(last) })

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

// A RevalidateAfterCommit whose notification is lost, because the method
// commits while the listener is down, is caught up by the reconnect tick:
// the revoked sub still ends, ahead of the method's res.
func TestRevalidateAfterCommitLostWhileListenerDown(t *testing.T) {
	var members string
	appName := "cometgo_test_" + newID(4)
	e := setupFull(t, namedPool(t, appName), dbTeam(t, &members))
	e.waitListening(t)
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	a.send(map[string]any{"t": "sub", "s": 1, "name": "dbteam"})
	if en := entries(a.next()); en[0]["ready"] != true {
		t.Fatalf("snapshot %v", en)
	}

	before := e.listened()
	e.killListener(t, appName)

	// Commits inside the listener's 500ms reconnect backoff.
	a.send(map[string]any{"t": "call", "id": 1, "m": "leave", "args": map[string]any{"User": "alice"}})
	if en := entries(a.next()); len(en) != 1 || en[0]["s"] != float64(1) || !isForbiddenEnd(en[0]) {
		t.Fatalf("expected the end before res, got %v", en)
	}
	if r := a.next(); r["t"] != "res" || r["ok"] != true || r["lag"] == true {
		t.Fatalf("leave res %v", r)
	}
	if e.listened() == before {
		t.Fatal("listener did not reconnect")
	}
}

// waitConn polls cond under c.mu until it holds, failing after 5s.
func waitConn(t *testing.T, c *conn, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		c.mu.Lock()
		ok := cond()
		c.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session never reached %s", what)
		}
	}
}

// namedPool gives the app its own pool tagged with appName, so a test can
// find and kill the app's LISTEN backend (killListener).
func namedPool(t *testing.T, appName string) func(*Config) {
	return func(c *Config) {
		cfg := c.DB.Config()
		cfg.ConnConfig.RuntimeParams["application_name"] = appName
		pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		c.DB = pool
	}
}

// killListener terminates the LISTEN backend of the app whose pool is
// tagged appName and waits until it has exited. The engine reconnects after
// a 500ms backoff.
func (e *testEnv) killListener(t *testing.T, appName string) {
	t.Helper()
	ctx := context.Background()
	var pid int32
	if err := e.pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity
		WHERE application_name = $1 AND query LIKE 'LISTEN %'`, appName).Scan(&pid); err != nil {
		t.Fatalf("find listener: %v", err)
	}
	if _, err := e.pool.Exec(ctx, "SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		var alive bool
		if err := e.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)", pid).Scan(&alive); err != nil {
			t.Fatal(err)
		}
		if !alive {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("listener backend did not exit")
		}
	}
}

// Revocation through a join table (docs/server.md, Authorization): deleting
// the membership row empties the user's result, also when it commits while
// the listener is down, once the listener is back.
func TestJoinTableRevocation(t *testing.T) {
	var members string
	appName := "cometgo_test_" + newID(4)
	e := setupFull(t, namedPool(t, appName), func(env *testEnv, app *App) {
		members = env.table + "_m"
		ctx := context.Background()
		if _, err := env.pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE %s (team text, user_id text, PRIMARY KEY (team, user_id))`, members)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { env.pool.Exec(context.Background(), "DROP TABLE "+members) })
		if err := InstallTriggers(ctx, env.pool, members); err != nil {
			t.Fatal(err)
		}
		Publish(app, "joined", PubOptions{Tables: []string{env.table, members}}, func(c *SubCtx, _ struct{}) (Query, error) {
			if c.Identity == nil {
				return Query{}, ErrForbidden
			}
			return Query{SQL: "SELECT i.id, i.name FROM " + env.table + " i JOIN " + members +
				" m ON m.team = i.owner WHERE m.user_id = $1 ORDER BY i.id", Args: []any{c.Identity.UserID}}, nil
		})
	})
	ctx := context.Background()
	exec := func(sql string) {
		t.Helper()
		if _, err := e.pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO " + e.table + " (owner, name) VALUES ('t1', 'secret')")
	grant := "INSERT INTO " + members + " VALUES ('t1', 'alice')"
	revoke := "DELETE FROM " + members + " WHERE user_id = 'alice'"
	exec(grant)
	e.waitListening(t)

	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	rows := subRows{}
	a.send(map[string]any{"t": "sub", "s": 1, "name": "joined"})
	rows.apply(t, a.next())
	// waitRows reads tx until sub 1 holds n rows; an end fails the test.
	waitRows := func(n int, what string) {
		t.Helper()
		for len(rows[1]) != n {
			m := a.next()
			if en := entries(m); len(en) > 0 && en[0]["end"] != nil {
				t.Fatalf("%s: sub ended: %v", what, en[0])
			}
			rows.apply(t, m)
		}
	}
	waitRows(1, "snapshot")

	exec(revoke)
	waitRows(0, "live revoke")
	exec(grant)
	waitRows(1, "grant")

	before := e.listened()
	e.killListener(t, appName)
	exec(revoke) // inside the listener's reconnect backoff: its notification is lost
	if e.listened() != before {
		t.Fatal("listener reconnected before the revoke committed; the lost-notification path was not exercised")
	}
	waitRows(0, "revoke while the listener was down")
	if e.listened() == before {
		t.Fatal("listener did not reconnect")
	}
}

// Revalidate requests drained in one tick are merged: fn runs once per sub
// however many requests name the user.
func TestRevalidateDedupesPerTick(t *testing.T) {
	var calls atomic.Int64
	e := setupFull(t, nil, func(env *testEnv, app *App) {
		Publish(app, "counted", PubOptions{Tables: []string{env.table}, Scope: ScopeShared},
			func(c *SubCtx, _ struct{}) (Query, error) {
				calls.Add(1)
				return Query{SQL: "SELECT id FROM " + env.table + " ORDER BY id"}, nil
			})
	})
	a, _ := e.dial(t, map[string]any{"auth": map[string]any{"token": "alice"}})
	subscribe(t, a, 1, "counted")
	subscribe(t, a, 2, "counted")
	calls.Store(0)

	// Queued under one lock, so a single tick drains all of them.
	eng := e.app.engine
	eng.mu.Lock()
	for range 50 {
		eng.nextN++
		eng.pending = append(eng.pending, notification{n: eng.nextN, reval: "alice"})
	}
	eng.nextN++
	eng.pending = append(eng.pending, notification{n: eng.nextN, reval: "bob"})
	eng.mu.Unlock()
	eng.signal()
	// The call's fence is released by a tick that drained the requests.
	a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "x", "Name": "n"}})
	for m := a.next(); m["t"] != "res"; m = a.next() {
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("fn ran %d times for 2 subs, want 2", n)
	}
}

// An over-long user ID is refused with ErrUserIDTooLong before anything is
// sent, so the method's transaction stays usable and its writes commit.
func TestRevalidateUserIDTooLong(t *testing.T) {
	long := strings.Repeat("u", maxNotifyPayload+1)
	e := setupFull(t, nil, func(env *testEnv, app *App) {
		Method(app, "revalLong", func(c *Call, _ struct{}) (string, error) {
			if err := c.RevalidateAfterCommit(long); !errors.Is(err, ErrUserIDTooLong) {
				return "", fmt.Errorf("RevalidateAfterCommit: %v", err)
			}
			if _, err := c.Tx.Exec(c.Context, "INSERT INTO "+env.table+" (owner, name) VALUES ('x', 'after-long')"); err != nil {
				return "", err
			}
			return "ok", nil
		})
	})
	if err := e.app.Revalidate(context.Background(), long); !errors.Is(err, ErrUserIDTooLong) {
		t.Fatalf("Revalidate: %v", err)
	}
	if err := e.app.Revalidate(context.Background(), strings.Repeat("u", maxNotifyPayload)); err != nil {
		t.Fatalf("Revalidate at the limit: %v", err)
	}
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "call", "id": 1, "m": "revalLong", "noFence": true})
	if got := okRes(t, a.next(), 1); got != "ok" {
		t.Fatalf("res %v", got)
	}
	if n := e.countRows(t, "SELECT count(*) FROM "+e.table+" WHERE name = 'after-long'"); n != 1 {
		t.Fatalf("method's write: %d rows", n)
	}
}

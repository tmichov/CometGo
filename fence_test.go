package cometgo

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func released(f *fence) bool {
	select {
	case <-f.released:
		return true
	default:
		return false
	}
}

// fenceEngine is an engine with no observers, so a tick takes no snapshot
// and needs no database.
func fenceEngine() *engine { return newEngine(&App{nodeID: "n"}) }

// reconnect runs one listener connection attempt that succeeds.
func reconnect(e *engine) { e.listening() }

// A fence whose notification was lost while nobody listened (here: before
// the first LISTEN) is released by the all-dirty tick after LISTEN.
func TestFenceCommittedBeforeListenReleasedByReconnectTick(t *testing.T) {
	e := fenceEngine()
	token, f := e.registerFence(nil)
	e.markCommitted(token)
	e.tick(context.Background())
	if released(f) {
		t.Fatal("released with no notification and no reconnect")
	}
	reconnect(e)
	e.tick(context.Background())
	if !released(f) {
		t.Fatal("committed fence not released by the reconnect tick")
	}
	if len(e.fences) != 0 {
		t.Fatalf("fences left registered: %v", e.fences)
	}
}

// An uncommitted fence is not released by a reconnect tick: the tick's
// snapshot could miss its writes (§6.2).
func TestUncommittedFenceSurvivesReconnectTick(t *testing.T) {
	e := fenceEngine()
	token, f := e.registerFence(nil)
	reconnect(e)
	e.tick(context.Background())
	if released(f) {
		t.Fatal("uncommitted fence released by a reconnect tick")
	}
	e.markCommitted(token)
	e.notify(parseNotification("1|f|" + token))
	e.tick(context.Background())
	if !released(f) {
		t.Fatal("fence not released by its notification")
	}
}

// Only a reconnect releases committed fences. Any other all-dirty tick
// (here an unparseable payload) leaves them to their own notification,
// which follows their events: releasing them early would send res ahead
// of evt.
func TestOtherAllDirtyTickKeepsCommittedFence(t *testing.T) {
	e := fenceEngine()
	reconnect(e)
	e.tick(context.Background())
	token, f := e.registerFence(nil)
	e.markCommitted(token)
	e.notify(parseNotification("garbage"))
	e.tick(context.Background())
	if released(f) {
		t.Fatal("committed fence released by a non-reconnect all-dirty tick")
	}
	e.notify(parseNotification("1|f|" + token))
	e.tick(context.Background())
	if !released(f) {
		t.Fatal("fence not released by its notification")
	}
}

// A fence committed after a LISTEN is not released by that LISTEN's tick,
// even when the tick drains after the commit: its notifications reach the
// new connection after the reconnect's, so releasing it there could send
// its res ahead of its events.
func TestFenceCommittedAfterListenIsNotSwept(t *testing.T) {
	e := fenceEngine()
	token, f := e.registerFence(nil)
	reconnect(e)
	e.markCommitted(token)
	e.tick(context.Background())
	if released(f) {
		t.Fatal("fence committed after LISTEN was swept by its tick")
	}
	// Lost to a later reconnect after all: that one releases it.
	reconnect(e)
	e.tick(context.Background())
	if !released(f) {
		t.Fatal("fence not released by the next reconnect")
	}
}

// A fence released by its own notification before commitFenced marks it
// committed is left alone.
func TestFenceCommittedAfterReleaseIsNoop(t *testing.T) {
	e := fenceEngine()
	token, f := e.registerFence(nil)
	reconnect(e)
	e.notify(parseNotification("1|f|" + token))
	e.tick(context.Background())
	if !released(f) {
		t.Fatal("not released")
	}
	e.markCommitted(token)
	if len(e.pending) != 0 || len(e.fences) != 0 {
		t.Fatalf("pending %v fences %v", e.pending, e.fences)
	}
}

// A call made right after Start, before LISTEN is up, resolves without lag.
func TestCallRightAfterStartHasNoLag(t *testing.T) {
	e := setup(t)
	a, _ := e.dial(t, nil)
	start := time.Now()
	a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "a", "Name": "x"}})
	res := a.next()
	if res["t"] != "res" || res["ok"] != true || res["lag"] == true {
		t.Fatalf("res %v", res)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("call took %v", d)
	}
}

// withAppName gives the app its own pool whose connections, the
// listener's included, carry application_name name, so a test can find
// them in pg_stat_activity.
func withAppName(t testing.TB, name string) func(*Config) {
	return func(c *Config) {
		cfg := c.DB.Config()
		cfg.ConnConfig.RuntimeParams["application_name"] = name
		pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		c.DB = pool
	}
}

// A call made while the listener is reconnecting resolves after the
// reconnect's full re-run, with its tx ahead of its res, not after
// FenceTimeout.
func TestCallDuringListenerReconnectHasNoLag(t *testing.T) {
	skipLogical(t, "it kills the LISTEN backend")
	appName := "cometgo_test_" + newID(4)
	e := setupWith(t, withAppName(t, appName))
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	if tx := a.next(); tx["t"] != "tx" {
		t.Fatalf("snapshot %v", tx)
	}

	ctx := context.Background()
	e.waitListening(t)
	before := e.listened()
	var pid int32
	err := e.pool.QueryRow(ctx, `SELECT pid FROM pg_stat_activity
		WHERE application_name = $1 AND query LIKE 'LISTEN %'`, appName).Scan(&pid)
	if err != nil {
		t.Fatalf("find listener: %v", err)
	}
	if _, err := e.pool.Exec(ctx, "SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatal(err)
	}
	// pg_terminate_backend only signals; wait until the backend is gone so
	// nothing committed from here on can reach it.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		var alive bool
		if err := e.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE pid = $1)", pid).Scan(&alive); err != nil {
			t.Fatal(err)
		}
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("listener backend did not exit")
		}
	}

	// The listener reconnects after a 500ms backoff; this call commits
	// inside that window, so its notifications are lost.
	start := time.Now()
	a.send(map[string]any{"t": "call", "id": 7, "m": "add", "args": map[string]any{"Owner": "a", "Name": "during"}})
	tx := a.next()
	if tx["t"] != "tx" {
		t.Fatalf("expected tx before res, got %v", tx)
	}
	if got := fmt.Sprint(entries(tx)[0]["ops"]); !strings.Contains(got, "during") {
		t.Fatalf("tx ops %s", got)
	}
	res := a.next()
	if res["t"] != "res" || res["ok"] != true || res["lag"] == true {
		t.Fatalf("res %v", res)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("call took %v, want well under FenceTimeout", d)
	}
	if e.listened() == before {
		t.Fatal("listener did not reconnect")
	}
}

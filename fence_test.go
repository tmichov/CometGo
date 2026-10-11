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

// A fence whose notification was lost while nobody listened (here: before
// the first LISTEN) is released by the all-dirty tick after LISTEN.
func TestFenceCommittedBeforeListenReleasedByReconnectTick(t *testing.T) {
	e := fenceEngine()
	token, f := e.registerFence(nil)
	e.fenceCommitted(token)
	e.tick(context.Background())
	if released(f) {
		t.Fatal("released with no notification and no all-dirty tick")
	}
	e.listening()
	e.tick(context.Background())
	if !released(f) {
		t.Fatal("committed fence not released by the reconnect tick")
	}
	if len(e.fences) != 0 {
		t.Fatalf("fences left registered: %v", e.fences)
	}
}

// An uncommitted fence is not released by an all-dirty tick: the tick's
// snapshot could miss its writes (§6.2).
func TestUncommittedFenceSurvivesAllDirtyTick(t *testing.T) {
	e := fenceEngine()
	token, f := e.registerFence(nil)
	e.notify(parseNotification("garbage")) // unparseable: all-dirty
	e.tick(context.Background())
	if released(f) {
		t.Fatal("uncommitted fence released by an all-dirty tick")
	}
	// Committed with the listener generation unchanged: its notification
	// is on its way, so no extra tick is queued.
	e.fenceCommitted(token)
	if len(e.pending) != 0 {
		t.Fatalf("pending %v, want none", e.pending)
	}
	e.notify(parseNotification("1|f|" + token))
	e.tick(context.Background())
	if !released(f) {
		t.Fatal("fence not released by its notification")
	}
}

// A fence whose transaction commits after a reconnect tick already drained
// may have lost its notification to that reconnect; committing it queues
// another all-dirty tick, which releases it.
func TestFenceCommittedAfterReconnectTickQueuesAnother(t *testing.T) {
	e := fenceEngine()
	e.listening()
	e.tick(context.Background())
	token, f := e.registerFence(nil)
	e.listening() // the listener dropped and came back before the commit
	e.tick(context.Background())
	if released(f) {
		t.Fatal("uncommitted fence released")
	}
	e.fenceCommitted(token)
	e.tick(context.Background())
	if !released(f) {
		t.Fatal("fence not released after commit following a reconnect")
	}
}

// A fence released by its own notification before runCall marks it
// committed is left alone.
func TestFenceCommittedAfterReleaseIsNoop(t *testing.T) {
	e := fenceEngine()
	token, f := e.registerFence(nil)
	e.listening()
	e.notify(parseNotification("1|f|" + token))
	e.tick(context.Background())
	if !released(f) {
		t.Fatal("not released")
	}
	e.fenceCommitted(token)
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

// A call made while the listener is reconnecting resolves after the
// reconnect's full re-run, with its tx ahead of its res, not after
// FenceTimeout.
func TestCallDuringListenerReconnectHasNoLag(t *testing.T) {
	appName := "cometgo_test_" + newID(4)
	e := setupWith(t, func(c *Config) {
		cfg := c.DB.Config()
		cfg.ConnConfig.RuntimeParams["application_name"] = appName
		pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		c.DB = pool
	})
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "items.all"})
	if tx := a.next(); tx["t"] != "tx" {
		t.Fatalf("snapshot %v", tx)
	}

	ctx := context.Background()
	e.waitListening(t)
	before := e.listenGen()
	var killed int
	err := e.pool.QueryRow(ctx, `SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
		WHERE application_name = $1 AND query LIKE 'LISTEN %'`, appName).Scan(&killed)
	if err != nil || killed != 1 {
		t.Fatalf("terminate listener: killed %d, err %v", killed, err)
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
	if e.listenGen() == before {
		t.Fatal("listener did not reconnect")
	}
}

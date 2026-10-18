package cometgo

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func waitReady(t *testing.T, app *App, ready bool, wantErr string) {
	t.Helper()
	var err error
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		err = app.Ready()
		if ready && err == nil || !ready && err != nil && strings.Contains(err.Error(), wantErr) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Ready: %v, want ready=%v %q", err, ready, wantErr)
		}
	}
}

func setReadyGrace(app *App, d time.Duration) {
	app.engine.feedMu.Lock()
	app.engine.readyGrace = d
	app.engine.feedMu.Unlock()
}

// Ready before Start and during Shutdown is an error.
func TestReadyLifecycle(t *testing.T) {
	e := setup(t)
	if err := New(Config{DB: e.pool}).Ready(); err == nil || !strings.Contains(err.Error(), "not started") {
		t.Errorf("before Start: %v", err)
	}
	e.waitListening(t)
	waitReady(t, e.app, true, "")
	if err := e.app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := e.app.Ready(); err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Errorf("after Shutdown: %v", err)
	}
}

// With the LISTEN connection's path cut, Ready stays nil for the grace a
// reconnect takes, then names the feed, and is nil again once it is back.
func TestReadyReportsListenerDown(t *testing.T) {
	skipLogical(t, "it cuts the LISTEN connection's path")
	e, proxy := setupProxied(t, nil)
	e.waitListening(t)
	waitReady(t, e.app, true, "")
	setReadyGrace(e.app, 500*time.Millisecond)
	proxy.pause()
	start := time.Now()
	waitReady(t, e.app, false, "change feed (LISTEN) down since")
	if d := time.Since(start); d < 500*time.Millisecond {
		t.Errorf("not ready after %v, within the grace", d)
	}
	proxy.resume()
	waitReady(t, e.app, true, "")
}

// A logical feed whose slot cannot be created yet (creation waits for a
// transaction holding an xid) is not ready, naming its slot; once the slot
// exists and streams, it is.
func TestReadyWaitsForLogicalSlot(t *testing.T) {
	var app *App
	release := make(chan struct{})
	e := setupFull(t, func(c *Config) {
		c.Feed, c.ReplicationDSN = FeedLogical, testDirectDSN()
	}, func(env *testEnv, a *App) {
		app = a
		if err := InstallPublication(context.Background(), env.pool, env.table); err != nil {
			t.Fatal(err)
		}
		tx, err := env.pool.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(context.Background(), "SELECT txid_current()"); err != nil {
			t.Fatal(err)
		}
		go func() {
			<-release
			tx.Commit(context.Background())
		}()
	})
	slot := app.engine.feed.(*logicalFeed).slot
	time.Sleep(300 * time.Millisecond) // the feed is creating its slot, waiting on the transaction
	if err := e.app.Ready(); err == nil || !strings.Contains(err.Error(), "logical replication slot "+slot) {
		t.Errorf("while the slot is being created: %v", err)
	}
	close(release)
	waitReady(t, e.app, true, "")
}

// A logical feed that streams but whose visibility gate cannot take
// snapshots (only its catalog connection is failing) delivers nothing: it
// is reported down after the grace, and up again once snapshots work.
func TestReadyReportsStuckGate(t *testing.T) {
	fe := newFeedEnv(t)
	var failing atomic.Bool
	real := fe.f.gate.snapshot
	fe.f.gate.snapshot = func(ctx context.Context) (pgSnapshot, error) {
		if failing.Load() {
			return pgSnapshot{}, errors.New("catalog connection refused")
		}
		return real(ctx)
	}
	setReadyGrace(fe.app, 300*time.Millisecond)
	ready := func() error { return fe.app.engine.feedReady(time.Now()) }
	fe.start()
	waitFor(t, "the feed up", func() bool { return ready() == nil })

	failing.Store(true)
	fe.insert(t, 1) // a transaction for the gate to hold
	waitFor(t, "the stuck gate reported", func() bool {
		err := ready()
		return err != nil && strings.Contains(err.Error(), "visibility gate: catalog connection refused")
	})
	if fe.ids(t)["1"] != 0 {
		t.Fatal("delivered while the gate had no snapshot")
	}
	failing.Store(false)
	waitFor(t, "id 1", func() bool { return fe.ids(t)["1"] == 1 })
	if err := ready(); err != nil {
		t.Errorf("after the gate recovered: %v", err)
	}
}

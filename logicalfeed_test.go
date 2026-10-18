package cometgo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
)

// feedEnv is a logical feed on its own engine, which never ticks: the
// test reads what the feed queued from engine.pending.
type feedEnv struct {
	*testEnv
	app  *App
	f    *logicalFeed
	stop context.CancelFunc
	done chan struct{}
}

func newFeedEnv(t *testing.T) *feedEnv {
	t.Helper()
	e := setup(t)
	if err := InstallPublication(context.Background(), e.pool, e.table); err != nil {
		t.Fatal(err)
	}
	app := New(Config{DB: e.pool, ReplicationDSN: os.Getenv("COMETGO_TEST_DATABASE_URL"), SlotName: "t" + e.table})
	fe := &feedEnv{testEnv: e, app: app, f: newLogicalFeed(app.engine)}
	t.Cleanup(func() {
		fe.halt()
		e.pool.Exec(context.Background(), `
			SELECT pg_terminate_backend(active_pid) FROM pg_replication_slots WHERE slot_name = $1 AND active_pid IS NOT NULL`, fe.f.slot)
		for range 50 {
			_, err := e.pool.Exec(context.Background(), `
				SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name = $1`, fe.f.slot)
			if err == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	return fe
}

func (fe *feedEnv) start() {
	ctx, cancel := context.WithCancel(context.Background())
	fe.stop, fe.done = cancel, make(chan struct{})
	go func() {
		defer close(fe.done)
		fe.f.run(ctx)
	}()
}

func (fe *feedEnv) halt() {
	if fe.stop != nil {
		fe.stop()
		<-fe.done
		fe.stop = nil
	}
}

// notes returns everything the feed has queued so far.
func (fe *feedEnv) notes() []notification {
	fe.app.engine.mu.Lock()
	defer fe.app.engine.mu.Unlock()
	return slices.Clone(fe.app.engine.pending)
}

// ids returns how many times each id of the test table was notified.
func (fe *feedEnv) ids(t *testing.T) map[string]int {
	got := map[string]int{}
	for _, n := range fe.notes() {
		if n.table != "public."+fe.table || n.keys == "" {
			continue
		}
		for _, k := range canonical(t, n.keys) {
			got[strings.Trim(k, `[]"`)]++
		}
	}
	return got
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (fe *feedEnv) insert(t *testing.T, ids ...int) {
	t.Helper()
	for _, id := range ids {
		execAll(t, fe.testEnv, fmt.Sprintf("INSERT INTO %s (id, owner, name) VALUES (%d, 'o', 'n')", fe.table, id))
	}
}

func (fe *feedEnv) slotPID(t *testing.T) (pid *int32) {
	t.Helper()
	if err := fe.pool.QueryRow(context.Background(), "SELECT active_pid FROM pg_replication_slots WHERE slot_name = $1", fe.f.slot).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return pid
}

// A killed connection resumes after the last commit received in full:
// nothing is lost, and the commits received before the kill, which the
// slot has not confirmed (the engine never drained them), are not sent
// again. Once the engine drains, the slot is confirmed.
func TestLogicalFeedResume(t *testing.T) {
	fe := newFeedEnv(t)
	fe.start()
	waitFor(t, "the slot", func() bool { return len(fe.notes()) > 0 }) // the new slot's all-dirty
	fe.insert(t, 1, 2)
	waitFor(t, "ids 1 and 2", func() bool { return fe.ids(t)["2"] == 1 })

	for range 3 {
		var pid *int32
		waitFor(t, "the slot active", func() bool { pid = fe.slotPID(t); return pid != nil })
		execAll(t, fe.testEnv, fmt.Sprintf("SELECT pg_terminate_backend(%d)", *pid))
		fe.insert(t, 10) // a write while it is down
		execAll(t, fe.testEnv, fmt.Sprintf("DELETE FROM %s WHERE id = 10", fe.table))
	}
	fe.insert(t, 3, 4, 5)
	waitFor(t, "id 5", func() bool { return fe.ids(t)["5"] >= 1 })
	got := fe.ids(t)
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		if got[id] != 1 {
			t.Errorf("id %s notified %d times, want once (%v)", id, got[id], got)
		}
	}
	// 10 is inserted and deleted three times: each insert and delete once.
	if got["10"] != 6 {
		t.Errorf("id 10 notified %d times, want 6", got["10"])
	}
	var written pglogrepl.LSN
	if err := fe.pool.QueryRow(context.Background(), "SELECT pg_current_wal_lsn()").Scan(&written); err != nil {
		t.Fatal(err)
	}
	confirmed := func() (c pglogrepl.LSN) {
		fe.pool.QueryRow(context.Background(), "SELECT confirmed_flush_lsn FROM pg_replication_slots WHERE slot_name = $1", fe.f.slot).Scan(&c)
		return c
	}
	if c := confirmed(); c >= written {
		t.Fatalf("slot confirmed %s before the engine drained (written %s): the resume skip was not exercised", c, written)
	}
	fe.app.engine.drained.Store(fe.app.engine.notifyAll(nil))
	waitFor(t, "the slot confirmed", func() bool { return confirmed() >= written })
}

// holdSlot streams the slot on a connection of its own, as a walsender the
// feed does not own, and returns its backend PID.
func holdSlot(t *testing.T, fe *feedEnv) *pgconn.PgConn {
	t.Helper()
	ctx := context.Background()
	c, err := fe.f.connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	if _, err := pglogrepl.CreateReplicationSlot(ctx, c, fe.f.slot, "pgoutput",
		pglogrepl.CreateReplicationSlotOptions{Mode: pglogrepl.LogicalReplication, SnapshotAction: "NOEXPORT_SNAPSHOT"}); err != nil {
		t.Fatal(err)
	}
	if err := pglogrepl.StartReplication(ctx, c, fe.f.slot, 0, pglogrepl.StartReplicationOptions{
		PluginArgs: []string{"proto_version '1'", "publication_names 'cometgo'"},
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

// A slot held by another process's walsender is in use: the feed backs
// off and never terminates it.
func TestLogicalFeedSlotInUse(t *testing.T) {
	fe := newFeedEnv(t)
	other := holdSlot(t, fe)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := fe.f.runOnce(ctx); !errors.Is(err, errSlotInUse) {
		t.Fatalf("runOnce: %v, want slot in use", err)
	}
	if pid := fe.slotPID(t); pid == nil || uint32(*pid) != other.PID() {
		t.Fatalf("slot now held by %v, want the other process's %d", pid, other.PID())
	}
}

// A walsender of our own previous connection that still holds the slot is
// terminated, and the feed then streams.
func TestLogicalFeedTerminatesZombie(t *testing.T) {
	fe := newFeedEnv(t)
	zombie := holdSlot(t, fe)
	fe.f.pid = int(zombie.PID()) // as if it were our last connection's
	fe.start()
	fe.insert(t, 1)
	waitFor(t, "id 1", func() bool { return fe.ids(t)["1"] == 1 })
	if pid := fe.slotPID(t); pid == nil || uint32(*pid) == zombie.PID() {
		t.Fatalf("slot held by %v; the zombie %d should be gone", pid, zombie.PID())
	}
}

// A slot dropped while the feed is down is created again, with an
// all-dirty notification for the changes it cannot replay.
func TestLogicalFeedRecreatesDroppedSlot(t *testing.T) {
	fe := newFeedEnv(t)
	fe.start()
	waitFor(t, "the slot", func() bool { return len(fe.notes()) > 0 })
	fe.halt()
	execAll(t, fe.testEnv, "SELECT pg_drop_replication_slot('"+fe.f.slot+"')")
	before := len(fe.notes())
	fe.start()
	// Changes before the new slot's consistent point are never decoded;
	// the all-dirty notification stands for them.
	waitFor(t, "the new slot", func() bool {
		return slices.ContainsFunc(fe.notes()[before:], func(n notification) bool { return n.all })
	})
	fe.insert(t, 7)
	waitFor(t, "id 7", func() bool { return fe.ids(t)["7"] == 1 })
}

// Shutdown drops the slot; a feed stopped any other way keeps it, to resume.
func TestLogicalFeedDropsSlotOnShutdown(t *testing.T) {
	fe := newFeedEnv(t)
	exists := func() (ok bool) {
		fe.pool.QueryRow(context.Background(), "SELECT EXISTS (SELECT FROM pg_replication_slots WHERE slot_name = $1)", fe.f.slot).Scan(&ok)
		return ok
	}
	fe.start()
	waitFor(t, "the slot", func() bool { return len(fe.notes()) > 0 })
	fe.halt()
	if !exists() {
		t.Fatal("slot dropped by a stop that was not a shutdown")
	}
	fe.start()
	fe.app.shutting.Store(true)
	fe.halt()
	if exists() {
		t.Error("slot still there after Shutdown")
	}
}

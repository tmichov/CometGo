package cometgo

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func slotExists(t *testing.T, fe *feedEnv, name string) (ok bool) {
	t.Helper()
	if err := fe.pool.QueryRow(context.Background(), "SELECT EXISTS (SELECT FROM pg_replication_slots WHERE slot_name = $1)", name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func createSlot(t *testing.T, fe *feedEnv, name string) {
	t.Helper()
	execAll(t, fe.testEnv, "SELECT pg_create_logical_replication_slot('"+name+"', 'pgoutput')")
	t.Cleanup(func() {
		fe.pool.Exec(context.Background(), "SELECT pg_drop_replication_slot(slot_name) FROM pg_replication_slots WHERE slot_name = $1", name)
	})
}

// The sweeper drops this database's cometgo_* slots inactive for
// OrphanSlotAfter (PG 17: inactive_since), and keeps an active one, one
// not inactive long enough, and the node's own.
func TestSweeperDropsOrphan(t *testing.T) {
	fe := newFeedEnv(t)
	ctx := context.Background()
	orphan, active := "cometgo_orph_"+fe.table, fe.f.slot
	createSlot(t, fe, orphan)
	holdSlot(t, fe) // fe's own slot name, held by a walsender: active
	other := "cometgo_act_" + fe.table
	fe.f.slot = other // the sweeping node's own slot is another one

	if err := fe.f.sweep(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if !slotExists(t, fe, orphan) {
		t.Fatal("slot dropped before OrphanSlotAfter")
	}
	if err := fe.f.sweep(ctx, fe.app.cfg.OrphanSlotAfter+time.Minute); err != nil {
		t.Fatal(err)
	}
	if slotExists(t, fe, orphan) {
		t.Error("orphan not dropped")
	}
	if !slotExists(t, fe, active) {
		t.Error("active slot dropped")
	}
}

// Before PG 17 (no inactive_since), an inactive slot counts from the first
// sweep that saw it with its LSN; an LSN that moved starts it again.
func TestSweeperKeepsSlotWhoseLSNMoved(t *testing.T) {
	fe := newFeedEnv(t)
	fe.f.noInactiveSince = true
	ctx := context.Background()
	slot := "cometgo_moved_" + fe.table
	createSlot(t, fe, slot)
	after := fe.app.cfg.OrphanSlotAfter
	if err := fe.f.sweep(ctx, 0); err != nil {
		t.Fatal(err)
	}
	fe.insert(t, 1) // WAL to advance over
	execAll(t, fe.testEnv, "SELECT pg_replication_slot_advance('"+slot+"', pg_current_wal_lsn())")
	if err := fe.f.sweep(ctx, after+time.Minute); err != nil {
		t.Fatal(err)
	}
	if !slotExists(t, fe, slot) {
		t.Fatal("slot whose LSN moved was dropped")
	}
	if err := fe.f.sweep(ctx, 2*after+2*time.Minute); err != nil {
		t.Fatal(err)
	}
	if slotExists(t, fe, slot) {
		t.Error("slot unchanged for OrphanSlotAfter not dropped")
	}
}

// Slots of another database on the cluster are never touched.
func TestSweeperKeepsOtherDatabase(t *testing.T) {
	fe := newFeedEnv(t)
	ctx := context.Background()
	db := "cometgo_other_" + fe.table
	execAll(t, fe.testEnv, "CREATE DATABASE "+db)
	cfg, err := pgx.ParseConfig(testDirectDSN())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = db
	other, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	slot := "cometgo_otherdb_" + fe.table
	t.Cleanup(func() {
		other.Exec(context.Background(), "SELECT pg_drop_replication_slot($1)", slot)
		other.Close(context.Background())
		fe.pool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+db)
	})
	if _, err := other.Exec(ctx, "SELECT pg_create_logical_replication_slot($1, 'pgoutput')", slot); err != nil {
		t.Fatal(err)
	}
	for _, fallback := range []bool{false, true} {
		fe.f.noInactiveSince = fallback
		for _, at := range []time.Duration{0, fe.app.cfg.OrphanSlotAfter + time.Minute} {
			if err := fe.f.sweep(ctx, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !slotExists(t, fe, slot) {
		t.Error("slot of another database dropped")
	}
}

// Creating the slot with max_replication_slots full fails with an error
// naming the cometgo slots that hold the places.
func TestSlotHeadroomCheck(t *testing.T) {
	fe := newFeedEnv(t)
	fe.waitListening(t) // the setup app's own slot, in logical mode, is counted
	ctx := context.Background()
	if err := fe.f.checkHeadroom(ctx); err != nil {
		t.Fatalf("with room: %v", err)
	}
	var used, limit int
	if err := fe.pool.QueryRow(ctx, "SELECT count(*), current_setting('max_replication_slots')::int FROM pg_replication_slots").Scan(&used, &limit); err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := range limit - used {
		name := fmt.Sprintf("cometgo_fill%d_%s", i, fe.table)
		createSlot(t, fe, name)
		names = append(names, name)
	}
	err := fe.f.checkHeadroom(ctx)
	if err == nil || !strings.Contains(err.Error(), "max_replication_slots") {
		t.Fatalf("full: %v", err)
	}
	for _, n := range names {
		if !strings.Contains(err.Error(), n+" (comet, inactive)") {
			t.Errorf("error does not name %s: %v", n, err)
		}
	}
}

// On PG 17 a slot's age is taken on the database's clock: a node whose own
// clock runs far ahead keeps a slot that has been inactive for seconds.
func TestSweeperUsesDatabaseClock(t *testing.T) {
	fe := newFeedEnv(t)
	slot := "cometgo_fresh_" + fe.table
	createSlot(t, fe, slot)
	fe.f.clock = func() time.Time { return time.Now().Add(time.Hour) }
	if err := fe.f.sweep(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	var v int
	if err := fe.pool.QueryRow(context.Background(), "SELECT current_setting('server_version_num')::int").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v >= 170000 && !slotExists(t, fe, slot) {
		t.Error("a slot inactive for seconds was dropped by a node whose clock runs an hour ahead")
	}
}

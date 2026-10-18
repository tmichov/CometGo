package cometgo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
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
	app := New(Config{DB: e.pool, ReplicationDSN: testDirectDSN(), SlotName: "t" + e.table})
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
// nothing is lost, and nothing received before the kill is sent again.
// The slot is confirmed up to what was received.
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
	// Confirmed up to what was received, with no drain needed: a fast
	// shutdown of Postgres waits for it.
	var written pglogrepl.LSN
	if err := fe.pool.QueryRow(context.Background(), "SELECT pg_current_wal_lsn()").Scan(&written); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the slot confirmed", func() bool {
		var c pglogrepl.LSN
		fe.pool.QueryRow(context.Background(), "SELECT confirmed_flush_lsn FROM pg_replication_slots WHERE slot_name = $1", fe.f.slot).Scan(&c)
		return c >= written
	})
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
	// The walsender may still hold the slot for a moment after the feed's
	// connection closed.
	waitFor(t, "the slot dropped", func() bool {
		_, err := fe.pool.Exec(context.Background(), "SELECT pg_drop_replication_slot($1)", fe.f.slot)
		return err == nil
	})
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

// A restart on snapshot S releases the fences whose xid is visible in S,
// and re-checks every sub; a fence not visible in S waits for its own
// message. The second pass's empty snapshot releases nothing.
func TestRestartReleasesFencesVisibleInS(t *testing.T) {
	e := fenceEngine()
	visTok, vis := e.registerFence(nil)
	e.setFenceXid(visTok, 5)
	lateTok, late := e.registerFence(nil)
	e.setFenceXid(lateTok, 50)
	unsetTok, unset := e.registerFence(nil) // fence statement not run yet
	s := pgSnapshot{xmin: 10, xmax: 40, xip: []uint64{20}}
	e.notifyAll([]notification{{all: true, restart: &pgSnapshot{}}})
	e.tick(context.Background())
	if released(vis) || released(late) || released(unset) {
		t.Fatal("the empty second-pass snapshot released a fence")
	}
	e.notifyAll([]notification{{all: true, restart: &s}})
	e.tick(context.Background())
	if !released(vis) {
		t.Error("fence visible in S not released")
	}
	if released(late) || released(unset) {
		t.Fatal("fence not visible in S released")
	}
	e.notify(notification{token: lateTok})
	e.tick(context.Background())
	if !released(late) {
		t.Error("late fence not released by its own message")
	}
	e.dropFence(unsetTok)
}

// Decoded transactions visible in the restart's S are dropped, since S
// covers them; later ones are delivered.
func TestDeliverDropsTransactionsVisibleInS(t *testing.T) {
	e := fenceEngine()
	f := newLogicalFeed(e)
	f.dropIn.Store(&pgSnapshot{xmin: 10, xmax: 40, xip: []uint64{20}})
	f.deliver(decodedTx{full: 5, end: 1, notes: []notification{{token: "old"}}})
	f.deliver(decodedTx{full: 20, end: 2, notes: []notification{{token: "in-xip"}}})
	f.deliver(decodedTx{full: 45, end: 3, notes: []notification{{token: "new"}}})
	var got []string
	for _, n := range e.pending {
		got = append(got, n.token)
	}
	if want := []string{"in-xip", "new"}; !slices.Equal(got, want) {
		t.Errorf("delivered %q, want %q", got, want)
	}
}

// A call whose transaction wrote before a recreated slot's consistent point
// and committed after it: slot creation waits for it, so it commits during
// the creation. Its res comes once, without lag, after its row and event
// (if the event comes at all: it is dropped when S covers its transaction),
// and the subscriber sees the row.
func TestRecreatedSlotSpanningCall(t *testing.T) {
	release := make(chan struct{})
	e := setupFull(t, func(c *Config) {
		c.Feed, c.ReplicationDSN = FeedLogical, testDirectDSN()
	}, func(env *testEnv, app *App) {
		if err := InstallPublication(context.Background(), env.pool, env.table); err != nil {
			t.Fatal(err)
		}
		Method(app, "holdAdd", func(c *Call, _ struct{}) (string, error) {
			var id string
			if err := c.Tx.QueryRow(c.Context, "INSERT INTO "+env.table+" (owner, name) VALUES ('h', 'held') RETURNING id::text").Scan(&id); err != nil {
				return "", err
			}
			if err := c.EmitAfterCommit("items.all", nil, "", "held", id); err != nil {
				return "", err
			}
			<-release
			return id, nil
		})
	})
	e.waitListening(t)
	slot := e.app.engine.feed.(*logicalFeed).slot
	a, _ := e.dial(t, evtCaps)
	subscribe(t, a, 1, "items.all")
	a.send(map[string]any{"t": "call", "id": 1, "m": "holdAdd", "args": map[string]any{}})
	waitFor(t, "the call's insert", func() bool {
		var n int
		e.pool.QueryRow(context.Background(), "SELECT count(*) FROM pg_locks WHERE locktype = 'transactionid' AND mode = 'ExclusiveLock' AND pid <> pg_backend_pid()").Scan(&n)
		return n > 0
	})

	// Drop the slot under the feed: terminate its walsender until the drop
	// gets in before the reconnect.
	before := e.listened()
	waitFor(t, "the slot dropped", func() bool {
		e.pool.Exec(context.Background(), "SELECT pg_terminate_backend(active_pid) FROM pg_replication_slots WHERE slot_name = $1 AND active_pid IS NOT NULL", slot)
		_, err := e.pool.Exec(context.Background(), "SELECT pg_drop_replication_slot($1)", slot)
		return err == nil
	})
	time.Sleep(300 * time.Millisecond) // the feed is now creating the slot, waiting for the call
	close(release)
	waitFor(t, "the feed restarted", func() bool { return e.listened() > before })

	var sawRow, sawEvt bool
	for {
		m := a.next()
		switch m["t"] {
		case "tx":
			sawRow = sawRow || strings.Contains(fmt.Sprint(m), "held")
		case "evt":
			if sawEvt {
				t.Fatal("event delivered twice")
			}
			sawEvt = true
		case "res":
			if m["ok"] != true || m["lag"] == true {
				t.Fatalf("res %v, want ok without lag", m)
			}
			if !sawRow {
				t.Fatal("res before the row reached the subscriber")
			}
			t.Logf("event delivered: %v", sawEvt)
			return
		}
	}
}

// On a logical node while FeedMigrating, a reconnect of its comet_reval
// LISTEN re-checks every sub but neither marks observers dirty nor
// releases a fence by listener generation: a message fence is released
// only by its own message or by a restart's visibility.
func TestRevalOnlyListenReleasesNoFence(t *testing.T) {
	e := fenceEngine()
	e.feed, e.revalOnly = newLogicalFeed(e), true
	token, f := e.registerFence(nil)
	e.markCommitted(token)
	e.listening()
	if n := e.pending[len(e.pending)-1]; n.all || n.listen == 0 {
		t.Fatalf("revalOnly LISTEN queued %+v, want a re-check-only notification", n)
	}
	e.tick(context.Background())
	if released(f) {
		t.Fatal("fence released by a re-check LISTEN")
	}
	e.dropFence(token)
}

// largeTxThenCall commits a 200,000-row transaction, then a small fenced
// call, and returns how long the call took. The replication connection
// runs with logical_decoding_work_mem at its minimum, so the large
// transaction is streamed (or, without streaming, spilled to disk).
func largeTxThenCall(t *testing.T, streaming bool) (time.Duration, slotStats) {
	t.Helper()
	dsn := testDirectDSN()
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	e := setupFull(t, func(c *Config) {
		c.Feed, c.ReplicationDSN = FeedLogical, dsn+sep+"logical_decoding_work_mem=64kB"
	}, func(env *testEnv, app *App) {
		if err := InstallPublication(context.Background(), env.pool, env.table); err != nil {
			t.Fatal(err)
		}
		app.engine.feed.(*logicalFeed).noStreaming = !streaming
	})
	e.waitListening(t)
	a, _ := e.dial(t, nil)
	// 200,000 rows in one transaction, written over about two seconds, as
	// a long batch job would: streamed, they are decoded as they come.
	tx, err := e.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if _, err := tx.Exec(context.Background(), "INSERT INTO "+e.table+" (owner, name) SELECT 'big', 'x' FROM generate_series(1, 10000); SELECT pg_sleep(0.1)"); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	a.send(map[string]any{"t": "call", "id": 1, "m": "add", "args": map[string]any{"Owner": "o", "Name": "small"}})
	for {
		m := a.next()
		if m["t"] != "res" {
			continue
		}
		if m["ok"] != true || m["lag"] == true {
			t.Fatalf("small call: %v", m)
		}
		took := time.Since(start)
		// The walsender reports its slot's statistics when it is stopped;
		// read them once it has.
		slot := e.app.engine.feed.(*logicalFeed).slot
		execAll(t, e, "SELECT pg_terminate_backend(active_pid) FROM pg_replication_slots WHERE slot_name = '"+slot+"' AND active_pid IS NOT NULL")
		var st slotStats
		waitFor(t, "the slot's statistics", func() bool {
			e.pool.QueryRow(context.Background(), "SELECT stream_txns, spill_txns FROM pg_stat_replication_slots WHERE slot_name = $1", slot).Scan(&st.streamed, &st.spilled)
			return st.streamed+st.spilled > 0
		})
		return took, st
	}
}

type slotStats struct{ streamed, spilled int64 }

// A large transaction is streamed while it runs, so a small fenced call
// committed after it is not held while the large one is decoded at its
// commit, as it is without streaming.
func TestLargeTransactionStreamed(t *testing.T) {
	without, spilled := largeTxThenCall(t, false)
	with, streamed := largeTxThenCall(t, true)
	t.Logf("small call after a 200,000-row transaction: %v streamed %+v, %v not %+v", with, streamed, without, spilled)
	if streamed.streamed == 0 || streamed.spilled != 0 {
		t.Errorf("streaming feed: %+v, want the transaction streamed, not spilled", streamed)
	}
	if spilled.spilled == 0 {
		t.Errorf("protocol 1 feed: %+v, want the transaction spilled", spilled)
	}
	// Without streaming the whole transaction is decoded after its commit,
	// ahead of the call's; streamed, most of it was decoded while it ran.
	if with > without/2 {
		t.Errorf("small call took %v streamed, %v without streaming: not held for less than half the decode", with, without)
	}
}

// The second pass waits while any transaction in progress in S is still
// running, then queues exactly one all-dirty restart that releases no
// fence; it gives up when ctx ends.
func TestSecondPassWaitsForSInProgress(t *testing.T) {
	e := fenceEngine()
	f := newLogicalFeed(e)
	token, fc := e.registerFence(nil)
	e.setFenceXid(token, 20)
	s := pgSnapshot{xmin: 10, xmax: 40, xip: []uint64{20, 30}}
	var running atomic.Value // the xids still in progress, []uint64
	running.Store([]uint64{20, 30})
	snap := func(context.Context) (pgSnapshot, error) {
		return pgSnapshot{xmin: 10, xmax: 50, xip: running.Load().([]uint64)}, nil
	}
	queued := func() int {
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(e.pending)
	}
	done := make(chan struct{})
	go func() {
		f.secondPass(context.Background(), s, snap)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	running.Store([]uint64{30}) // 20 has ended, 30 has not
	time.Sleep(100 * time.Millisecond)
	if n := queued(); n != 0 {
		t.Fatalf("queued %d notifications while 30 was still running", n)
	}
	running.Store([]uint64{})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("second pass never ran")
	}
	if n := queued(); n != 1 {
		t.Fatalf("queued %d notifications, want one", n)
	}
	if n := e.pending[0]; !n.all || n.restart == nil || n.restart.visible(20) {
		t.Fatalf("queued %+v, want an all-dirty restart on an empty snapshot", n)
	}
	e.tick(context.Background())
	if released(fc) {
		t.Error("the second pass released a fence")
	}
	e.dropFence(token)

	// A ctx that ends while it waits stops it.
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		f.secondPass(ctx, pgSnapshot{xmax: 40, xip: []uint64{30}}, func(context.Context) (pgSnapshot, error) {
			return pgSnapshot{xmax: 50, xip: []uint64{30}}, nil
		})
		close(stopped)
	}()
	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("second pass kept waiting after ctx ended")
	}
}

// proxiedFeed is a feedEnv whose replication connection goes through a
// pauseProxy, with short liveness timings.
func proxiedFeed(t *testing.T) (*feedEnv, *pauseProxy) {
	t.Helper()
	fe := newFeedEnv(t)
	cc, err := pgconn.ParseConfig(testDirectDSN())
	if err != nil {
		t.Fatal(err)
	}
	proxy := newPauseProxy(t, net.JoinHostPort(cc.Host, strconv.Itoa(int(cc.Port))))
	t.Cleanup(proxy.resume) // before the feed's cleanup, so it can stop
	u, err := url.Parse(testDirectDSN())
	if err != nil {
		t.Fatal(err)
	}
	u.Host = proxy.ln.Addr().String()
	q := u.Query()
	q.Set("sslmode", "disable") // one connect attempt per try: no TLS, no fallback
	u.RawQuery = q.Encode()
	fe.app.cfg.ReplicationDSN = u.String()
	eng := fe.app.engine
	eng.listenIdle, eng.listenProbe, eng.listenConnect = 300*time.Millisecond, 200*time.Millisecond, 300*time.Millisecond
	return fe, proxy
}

// A replication connection whose path goes silent, without a TCP reset,
// is noticed by the probe within listenIdle + listenProbe and replaced
// once the path is back; writes made meanwhile arrive exactly once.
func TestLogicalFeedProbesSilentPath(t *testing.T) {
	fe, proxy := proxiedFeed(t)
	fe.start()
	waitFor(t, "the slot", func() bool { return fe.app.engine.starts.Load() > 0 })
	fe.insert(t, 1)
	waitFor(t, "id 1", func() bool { return fe.ids(t)["1"] == 1 })
	before := fe.app.engine.starts.Load()

	proxy.pause()
	fe.insert(t, 2, 3)      // while the path is dead
	time.Sleep(time.Second) // past listenIdle + listenProbe, and a few connect timeouts
	proxy.resume()
	waitFor(t, "the feed back", func() bool { return fe.app.engine.starts.Load() > before })
	waitFor(t, "ids 2 and 3", func() bool { return fe.ids(t)["3"] >= 1 })
	for _, id := range []string{"1", "2", "3"} {
		if n := fe.ids(t)[id]; n != 1 {
			t.Errorf("id %s notified %d times, want once", id, n)
		}
	}
}

// A quiet connection on a live path is probed and answered: no reconnect.
func TestLogicalFeedQuietPathStays(t *testing.T) {
	fe, _ := proxiedFeed(t)
	fe.start()
	waitFor(t, "the slot", func() bool { return fe.app.engine.starts.Load() > 0 })
	before := fe.app.engine.starts.Load()
	time.Sleep(2 * time.Second) // several listenIdle periods without a change
	if n := fe.app.engine.starts.Load(); n != before {
		t.Errorf("feed restarted %d times on a quiet, live path", n-before)
	}
}

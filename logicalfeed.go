package cometgo

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgproto3"
)

// logicalFeed reads the node's replication slot (PROTOCOL.md §11.1,
// docs/design/m4-logical-feed.md §5): decoder parses it, gate hands
// visible transactions to the engine, and the slot is confirmed as the
// engine drains them.
type logicalFeed struct {
	e    *engine
	slot string // cometgo_<Config.SlotName>

	// Owned by run.
	received pglogrepl.LSN // end LSN of the last commit received in full
	pid      int           // our last walsender's backend PID, for zombies

	// catalog is the feed's own connection, for snapshots and key lookups:
	// a pool saturated by method calls must not stall the feed.
	catMu   sync.Mutex
	catalog *pgx.Conn

	gate *gate
	// pend holds, in order, each delivered transaction's end LSN with the
	// number of its last notification; confirmed is the end of the last
	// one the engine has drained. Guarded by pendMu.
	pendMu    sync.Mutex
	pend      []pendingConfirm
	confirmed pglogrepl.LSN

	// Timings, settable by tests.
	senderTimeout time.Duration // wal_sender_timeout on the connection
	statusEvery   time.Duration
	createTimeout time.Duration
}

type pendingConfirm struct {
	n   uint64
	end pglogrepl.LSN
}

func newLogicalFeed(e *engine) *logicalFeed {
	name := e.a.cfg.SlotName
	if name == "" {
		name = e.a.nodeID
	}
	f := &logicalFeed{
		e:             e,
		slot:          "cometgo_" + name,
		senderTimeout: 10 * time.Second,
		statusEvery:   time.Second,
		createTimeout: 30 * time.Second,
	}
	f.gate = newGate(f.snapshot, f.deliver)
	return f
}

// withCatalog runs fn on the catalog connection, connecting it first if
// needed, and drops the connection when fn fails, so the next call
// reconnects.
func (f *logicalFeed) withCatalog(ctx context.Context, fn func(*pgx.Conn) error) error {
	f.catMu.Lock()
	defer f.catMu.Unlock()
	if f.catalog == nil {
		cfg := f.e.a.db.Config().ConnConfig.Copy()
		if cfg.ConnectTimeout == 0 {
			cfg.ConnectTimeout = f.e.listenConnect
		}
		c, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			return err
		}
		f.catalog = c
	}
	if err := fn(f.catalog); err != nil {
		f.catalog.Close(context.Background())
		f.catalog = nil
		return err
	}
	return nil
}

func (f *logicalFeed) snapshot(ctx context.Context) (s pgSnapshot, err error) {
	err = f.withCatalog(ctx, func(c *pgx.Conn) error {
		s, err = currentSnapshot(ctx, c)
		return err
	})
	return s, err
}

func (f *logicalFeed) pkColumns(ctx context.Context, relID uint32) (cols []string, err error) {
	err = f.withCatalog(ctx, func(c *pgx.Conn) error {
		cols, err = pkColumns(ctx, c, relID)
		return err
	})
	return cols, err
}

// deliver hands a visible transaction to the engine and records its end
// LSN, to be confirmed once the engine has drained its notifications.
func (f *logicalFeed) deliver(tx decodedTx) {
	last := f.e.notifyAll(tx.notes)
	if tx.end == 0 {
		return // a non-transactional message: nothing to confirm
	}
	f.pendMu.Lock()
	f.pend = append(f.pend, pendingConfirm{last, tx.end})
	f.pendMu.Unlock()
}

// confirmable returns the LSN the slot may be confirmed up to: the end of
// the last delivered transaction the engine has drained. With nothing in
// flight (no transaction half received, none held or undrained), it is
// idle, and walEnd, the server's position from its last keepalive, is
// confirmed too: every transaction before it was sent before the
// keepalive, so writes to untracked tables do not pile up behind the slot.
func (f *logicalFeed) confirmable(midTx bool, walEnd pglogrepl.LSN) pglogrepl.LSN {
	drained := f.e.drained.Load()
	f.pendMu.Lock()
	defer f.pendMu.Unlock()
	for len(f.pend) > 0 && f.pend[0].n <= drained {
		f.confirmed = f.pend[0].end
		f.pend = f.pend[1:]
	}
	f.gate.mu.Lock()
	held := len(f.gate.queue)
	f.gate.mu.Unlock()
	if !midTx && held == 0 && len(f.pend) == 0 && walEnd > f.confirmed {
		f.confirmed = walEnd
	}
	return f.confirmed
}

func (f *logicalFeed) run(ctx context.Context) {
	go func() {
		for ctx.Err() == nil {
			if err := f.gate.run(ctx); err != nil && ctx.Err() == nil {
				f.e.a.log.Warn("logical feed: snapshot failed", "err", err)
				select {
				case <-ctx.Done():
				case <-time.After(100 * time.Millisecond):
				}
			}
		}
	}()
	// As engine.listen: every gap delays fenced calls, so the wait stays
	// short; one connection that stayed up resets it.
	const (
		minBackoff  = 500 * time.Millisecond
		maxBackoff  = 2 * time.Second
		stableAfter = 30 * time.Second
	)
	backoff := minBackoff
	for ctx.Err() == nil {
		startedAt, err := f.runOnce(ctx)
		if ctx.Err() != nil {
			break
		}
		if !startedAt.IsZero() && time.Since(startedAt) >= stableAfter {
			backoff = minBackoff
		}
		f.e.a.log.Warn("logical feed down", "slot", f.slot, "err", err, "retry", backoff)
		f.e.a.metric(MetricListenReconnect, "", 1)
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
	if f.e.a.shutting.Load() {
		f.dropSlot()
	}
	f.catMu.Lock()
	if f.catalog != nil {
		f.catalog.Close(context.Background())
		f.catalog = nil
	}
	f.catMu.Unlock()
}

func (f *logicalFeed) connect(ctx context.Context) (*pgconn.PgConn, error) {
	cfg, err := pgconn.ParseConfig(f.e.a.cfg.ReplicationDSN)
	if err != nil {
		return nil, err
	}
	cfg.RuntimeParams["replication"] = "database"
	// A short timeout ends a dead connection's walsender, which holds the
	// slot until it does (and we terminate our own zombies sooner).
	cfg.RuntimeParams["wal_sender_timeout"] = strconv.FormatInt(f.senderTimeout.Milliseconds(), 10)
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = f.e.listenConnect
	}
	// A receive that times out must leave the connection streaming: a read
	// deadline, never a cancel request, which would end the replication.
	cfg.BuildContextWatcherHandler = func(c *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.DeadlineContextWatcherHandler{Conn: c.Conn()}
	}
	return pgconn.ConnectConfig(ctx, cfg)
}

// errSlotInUse is a slot active for a walsender that is not ours: another
// live process uses the same SlotName. It is never terminated, or two
// nodes would end each other's feed in turn.
var errSlotInUse = errors.New("slot in use by another process (two nodes with the same Config.SlotName?)")

// runOnce runs one replication connection until it fails. It reports when
// replication started, or the zero time if it did not.
func (f *logicalFeed) runOnce(ctx context.Context) (startedAt time.Time, err error) {
	defer recoverErr(&err)
	conn, err := f.connect(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer conn.Close(context.Background())
	prevPID := f.pid
	f.pid = int(conn.PID())

	rows, err := conn.Exec(ctx, "SELECT active_pid FROM pg_replication_slots WHERE slot_name = '"+f.slot+"'").ReadAll()
	if err != nil {
		return time.Time{}, err
	}
	start := f.received
	switch {
	case len(rows[0].Rows) == 0:
		// Missing: new, dropped by a sweep, or its WAL removed. Its
		// consistent point is where decoding starts; changes before it are
		// covered by an all-dirty tick (the rule of CMTG-87).
		cctx, cancel := context.WithTimeout(ctx, f.createTimeout)
		res, err := pglogrepl.CreateReplicationSlot(cctx, conn, f.slot, "pgoutput",
			pglogrepl.CreateReplicationSlotOptions{Mode: pglogrepl.LogicalReplication, SnapshotAction: "NOEXPORT_SNAPSHOT"})
		cancel()
		if err != nil {
			return time.Time{}, fmt.Errorf("create slot %s: %w", f.slot, err)
		}
		if start, err = pglogrepl.ParseLSN(res.ConsistentPoint); err != nil {
			return time.Time{}, err
		}
		f.e.a.log.Info("logical feed: slot created", "slot", f.slot, "consistent_point", res.ConsistentPoint)
		f.received = start
		f.e.notifyAll([]notification{{all: true}})
	case rows[0].Rows[0][0] != nil:
		active, _ := strconv.Atoi(string(rows[0].Rows[0][0]))
		if active != prevPID || prevPID == 0 {
			return time.Time{}, fmt.Errorf("%w (walsender pid %d)", errSlotInUse, active)
		}
		// Our previous connection's walsender has not noticed it is gone:
		// end it rather than wait out wal_sender_timeout.
		if _, err := conn.Exec(ctx, fmt.Sprintf("SELECT pg_terminate_backend(%d)", active)).ReadAll(); err != nil {
			return time.Time{}, err
		}
		return time.Time{}, fmt.Errorf("terminated our previous walsender %d, which still held the slot", active)
	}

	// From the end of the last commit received in full: Postgres skips
	// commits below a start above confirmed_flush, so nothing the engine
	// already has comes again, and a half-received transaction comes whole.
	// 0 (a fresh process on an existing slot) means confirmed_flush.
	err = pglogrepl.StartReplication(ctx, conn, f.slot, start, pglogrepl.StartReplicationOptions{
		PluginArgs: []string{"proto_version '1'", "publication_names '" + pubName + "'", "messages 'true'"},
	})
	if err != nil {
		return time.Time{}, err
	}
	startedAt = time.Now()
	return startedAt, f.stream(ctx, conn)
}

// stream reads replication messages until the connection fails.
func (f *logicalFeed) stream(ctx context.Context, conn *pgconn.PgConn) error {
	d := newDecoder(f.pkColumns)
	var walEnd, sent pglogrepl.LSN
	lastRecv := time.Now()
	var probeBy time.Time // a reply to our probe is due by then
	nextStatus := time.Now()
	reply := false
	for {
		now := time.Now()
		if !probeBy.IsZero() && now.After(probeBy) {
			return errors.New("logical feed: no reply to a status probe")
		}
		if probeBy.IsZero() && now.Sub(lastRecv) >= f.e.listenIdle {
			// Quiet for listenIdle: ask for a reply. Server keepalives come
			// every wal_sender_timeout / 2 on their own, but a path that died
			// without a TCP reset sends nothing.
			probeBy = now.Add(f.e.listenProbe)
			reply = true
		}
		if c := f.confirmable(d.xid != 0, walEnd); reply || c != sent && !now.Before(nextStatus) {
			err := pglogrepl.SendStandbyStatusUpdate(ctx, conn, pglogrepl.StandbyStatusUpdate{
				WALWritePosition: c, WALFlushPosition: c, WALApplyPosition: c,
				ReplyRequested: !probeBy.IsZero(),
			})
			if err != nil {
				return err
			}
			sent, reply = c, false
		}
		if !now.Before(nextStatus) {
			nextStatus = now.Add(f.statusEvery)
		}
		deadline := nextStatus
		if !probeBy.IsZero() && probeBy.Before(deadline) {
			deadline = probeBy
		}
		if idleAt := lastRecv.Add(f.e.listenIdle); probeBy.IsZero() && idleAt.Before(deadline) {
			deadline = idleAt
		}
		rctx, cancel := context.WithDeadline(ctx, deadline)
		raw, err := conn.ReceiveMessage(rctx)
		cancel()
		if err != nil && ctx.Err() == nil && pgconn.Timeout(err) {
			continue
		}
		if err != nil {
			return err
		}
		lastRecv, probeBy = time.Now(), time.Time{}
		switch m := raw.(type) {
		case *pgproto3.ErrorResponse:
			return pgconn.ErrorResponseToPgError(m)
		case *pgproto3.CopyData:
			switch m.Data[0] {
			case pglogrepl.PrimaryKeepaliveMessageByteID:
				k, err := pglogrepl.ParsePrimaryKeepaliveMessage(m.Data[1:])
				if err != nil {
					return err
				}
				walEnd = k.ServerWALEnd
				reply = reply || k.ReplyRequested
			case pglogrepl.XLogDataByteID:
				x, err := pglogrepl.ParseXLogData(m.Data[1:])
				if err != nil {
					return err
				}
				msg, err := pglogrepl.Parse(x.WALData)
				if err != nil {
					return err
				}
				tx, err := d.handle(ctx, msg)
				if err != nil {
					return err
				}
				if tx != nil {
					f.gate.push(*tx)
					if tx.end != 0 {
						f.received = tx.end
					}
				}
			}
		}
	}
}

// dropSlot drops the slot on a clean shutdown; a crashed process leaves
// it for the sweeper (CMTG-88). WAIT lets our walsender, ended a moment
// ago, release it first.
func (f *logicalFeed) dropSlot() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := f.connect(ctx)
	if err == nil {
		defer conn.Close(context.Background())
		err = pglogrepl.DropReplicationSlot(ctx, conn, f.slot, pglogrepl.DropReplicationSlotOptions{Wait: true})
	}
	if err != nil {
		f.e.a.log.Warn("logical feed: slot not dropped; the orphan sweeper will drop it", "slot", f.slot, "err", err)
	}
}

// validSlotName reports whether Config.SlotName can name a slot:
// cometgo_ plus it must be a replication slot name (lowercase letters,
// digits and underscores, at most 63 bytes).
func validSlotName(s string) bool {
	return len(s) <= 63-len("cometgo_") && strings.Trim(s, "abcdefghijklmnopqrstuvwxyz0123456789_") == ""
}

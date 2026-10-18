package cometgo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgproto3"
)

// logicalFeed reads the node's replication slot (PROTOCOL.md §11.1,
// docs/design/m4-logical-feed.md §5): decoder parses it, gate hands
// visible transactions to the engine, and the slot is confirmed up to what
// was received.
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

	// The feed is up for App.Ready while it streams and its gate can take
	// snapshots: a gate stuck on its catalog connection delivers nothing.
	healthMu  sync.Mutex
	streaming bool
	streamErr error
	gateErr   error
	// dropIn is the snapshot S of the last restart: decoded transactions
	// visible in it are dropped.
	dropIn    atomic.Pointer[pgSnapshot]
	confirmed pglogrepl.LSN // owned by stream

	// swept is what the last sweep saw of each inactive slot, when Postgres
	// has no inactive_since (before 17); noInactiveSince forces that path
	// (tests). Owned by the sweeper.
	swept           map[string]seenSlot
	noInactiveSince bool
	clock           func() time.Time // this node's clock, for swept (tests skew it)

	// Timings, settable by tests.
	senderTimeout time.Duration // wal_sender_timeout on the connection
	statusEvery   time.Duration
	createTimeout time.Duration
	noStreaming   bool // protocol 1, as before streaming (tests)
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
		clock:         time.Now,
	}
	f.gate = newGate(f.snapshot, f.deliver)
	return f
}

// withCatalog runs fn on the catalog connection, connecting it first if
// needed, and drops the connection when fn fails, so the next call
// reconnects.
func (f *logicalFeed) withCatalog(ctx context.Context, fn func(*pgx.Conn) error) (err error) {
	f.catMu.Lock()
	defer f.catMu.Unlock()
	defer func() {
		if err != nil && f.catalog != nil {
			f.catalog.Close(context.Background())
			f.catalog = nil
		}
	}()
	// The connection copies the pool's ConnConfig, tracer included, so user
	// code runs here too: a panic in it fails this call like a network
	// error, instead of ending the process from the gate's goroutine.
	defer recoverErr(&err)
	if f.catalog == nil {
		cfg := f.e.a.db.Config().ConnConfig.Copy()
		if cfg.ConnectTimeout == 0 {
			cfg.ConnectTimeout = f.e.listenConnect
		}
		if f.catalog, err = pgx.ConnectConfig(ctx, cfg); err != nil {
			f.catalog = nil
			return err
		}
	}
	return fn(f.catalog)
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

// deliver hands a visible transaction to the engine.
// health updates the feed's state with fn, under healthMu, and reports
// it to the engine.
func (f *logicalFeed) health(fn func()) {
	f.healthMu.Lock()
	defer f.healthMu.Unlock()
	fn()
	switch {
	case !f.streaming:
		f.e.feedIsDown(f.streamErr)
	case f.gateErr != nil:
		f.e.feedIsDown(fmt.Errorf("visibility gate: %w", f.gateErr))
	default:
		f.e.feedIsUp()
	}
}

func (f *logicalFeed) deliver(tx decodedTx) {
	if tx.xid != 0 && f.gateBlocked() {
		f.health(func() { f.gateErr = nil }) // its snapshot just worked
	}
	if s := f.dropIn.Load(); s != nil && tx.full != 0 && s.visible(tx.full) {
		return // covered by the restart on S
	}
	f.e.notifyAll(tx.notes)
}

// confirmable returns the LSN the slot is confirmed up to: the end of the
// last commit received in full and, outside a transaction, walEnd, the
// server's position from its last keepalive (every transaction before it
// was sent before the keepalive), so writes to untracked tables do not pile
// up behind the slot. Received is enough: a reconnect resumes from it with
// the gate's queue kept, and the engine's state dies with the process. And
// it must be: a walsender ends at a fast shutdown only once its client has
// confirmed everything sent, so confirming only what the engine drained
// held Postgres' shutdown until it was killed.
func (f *logicalFeed) confirmable(midTx bool, walEnd pglogrepl.LSN) pglogrepl.LSN {
	f.confirmed = max(f.confirmed, f.received)
	if !midTx {
		f.confirmed = max(f.confirmed, walEnd)
	}
	return f.confirmed
}

func (f *logicalFeed) gateBlocked() bool {
	f.healthMu.Lock()
	defer f.healthMu.Unlock()
	return f.gateErr != nil
}

func (f *logicalFeed) run(ctx context.Context) {
	go f.sweepLoop(ctx)
	if f.e.revalOnly {
		go f.e.listen(ctx) // re-checks from notify-mode nodes (FeedMigrating)
	}
	go func() {
		for ctx.Err() == nil {
			if err := f.gate.run(ctx); err != nil && ctx.Err() == nil {
				f.health(func() { f.gateErr = err })
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
		f.health(func() { f.streaming, f.streamErr = false, err })
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
		// covered by an all-dirty tick (restart).
		if err := f.checkHeadroom(ctx); err != nil {
			return time.Time{}, err
		}
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
		if err := f.restart(ctx); err != nil {
			return time.Time{}, err
		}
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
	// Protocol 2 streams a large transaction while it runs (design §8),
	// instead of decoding it whole at its commit while every later commit
	// waits behind it.
	args := []string{"proto_version '2'", "streaming 'on'", "publication_names '" + pubName + "'", "messages 'true'"}
	if f.noStreaming {
		args = []string{"proto_version '1'", "publication_names '" + pubName + "'", "messages 'true'"}
	}
	err = pglogrepl.StartReplication(ctx, conn, f.slot, start, pglogrepl.StartReplicationOptions{PluginArgs: args})
	if err != nil {
		return time.Time{}, err
	}
	startedAt = time.Now()
	f.e.starts.Add(1)
	f.health(func() { f.streaming = true })
	return startedAt, f.stream(ctx, conn)
}

// restart covers what a new slot cannot replay (design §6): changes
// committed before its consistent point are never decoded. On a snapshot S
// taken after the slot exists, the engine marks everything dirty,
// re-checks every sub and releases the fences visible in S; decoded
// messages of transactions visible in S are dropped (deliver), since S
// covers their writes and their events could otherwise follow their res.
// A transaction not visible in S commits after it, so after the consistent
// point, and is decoded whole. One that was in a synchronous-standby wait
// at S may have committed below the consistent point: once every
// transaction in progress in S has ended, a second pass marks everything
// dirty and re-checks every sub again. Its fence, if any, lags.
func (f *logicalFeed) restart(ctx context.Context) error {
	s, err := f.snapshot(ctx)
	if err != nil {
		return err
	}
	f.dropIn.Store(&s)
	f.e.notifyAll([]notification{{all: true, restart: &s}})
	if len(s.xip) > 0 {
		go f.secondPass(ctx, s, f.snapshot)
	}
	return nil
}

// secondPass waits, polling snapshot, until every transaction in progress
// in s has ended, then queues the second all-dirty pass.
func (f *logicalFeed) secondPass(ctx context.Context, s pgSnapshot, snapshot func(context.Context) (pgSnapshot, error)) {
	for ctx.Err() == nil {
		cur, err := snapshot(ctx)
		if err == nil && !slices.ContainsFunc(s.xip, func(x uint64) bool { return !cur.visible(x) }) {
			// An empty snapshot releases no fence.
			f.e.notifyAll([]notification{{all: true, restart: &pgSnapshot{}}})
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (f *logicalFeed) fence(ctx context.Context, tx pgx.Tx, token string, always bool) (bool, error) {
	sql := "SELECT pg_logical_emit_message(true, 'comet', $1)::text, txid_current()"
	if !always {
		// The WHERE runs first: the message would assign the ID itself.
		sql += " WHERE txid_current_if_assigned() IS NOT NULL"
	}
	var lsn string
	var xid uint64
	err := tx.QueryRow(ctx, sql, "f|"+token).Scan(&lsn, &xid)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	f.e.setFenceXid(token, xid)
	return true, nil
}

// commitFenced needs no ordering against restarts: a fence is released by
// its decoded message, or by a restart in which its xid is visible.
func (f *logicalFeed) commitFenced(ctx context.Context, tx pgx.Tx, _ string) error {
	return tx.Commit(ctx)
}

// The listener-generation release is the notify feed's; the logical feed
// releases by visibility (restart).
func (*logicalFeed) restarted(uint64, uint64) {}

// msgSQL sends the message transactional, also in its own autocommit
// statement (Emit, Revalidate): a commit flushes the WAL at once, where a
// non-transactional message waits for the WAL writer.
func (*logicalFeed) msgSQL(kind byte) string {
	return "SELECT pg_logical_emit_message(true, 'comet', '" + string(kind) + "|' || $1)"
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
		if c := f.confirmable(d.xid != 0 || len(d.streams) > 0, walEnd); reply || c != sent && !now.Before(nextStatus) {
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
				msg, err := pglogrepl.ParseV2(x.WALData, d.inStream)
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

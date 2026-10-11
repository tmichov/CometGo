package cometgo

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The engine owns all observers and subscriptions. Only its run goroutine
// touches them; other goroutines talk to it through enqueue/notify and fences.
type engine struct {
	a *App

	mu      sync.Mutex
	nextN   uint64
	pending []notification
	reqs    []any
	fences  map[string]*fence
	fenceN  uint64
	wake    chan struct{}

	// Owned by the run goroutine.
	observers map[string]*observer
	conns     map[*conn]map[uint64]*subscription
	// ended records, per resumable session, the sub ids whose end was sent.
	// After a resume the client re-sends its subs before the replayed end
	// reaches it; re-binding such an id would leave a sub the client never
	// tracks again. Sub ids are never reused (§4.1), so a sub for one of
	// these is always that re-send.
	ended map[*conn]map[uint64]bool
	// coalescing holds the connections in coalescing mode (§8.1): their
	// queue went over softQueuedBytes, so their subs' changes are held as
	// needsReset until it drains below lowWaterBytes.
	coalescing   map[*conn]bool
	carryFences  []*fence
	carryEvts    []*evtNote
	carryOut     map[*conn]*connOut
	retryBackoff time.Duration
}

type notification struct {
	n     uint64
	all   bool   // listener (re)connected or unparseable: everything is dirty
	table string // r/t notifications
	token string // fence
	evt   *evtNote
}

type fence struct {
	released chan struct{}
	c        *conn // the connection whose res waits on it
}

type observer struct {
	key    string
	pub    *publication
	q      Query
	tables []string

	cols   []colSpec
	keyIdx []int
	res    *result

	subs  map[*subscription]struct{}
	dirty bool

	// Per-tick output.
	ops []op
	err *Error
}

type subscription struct {
	c       *conn
	s       uint64
	name    string
	args    json.RawMessage
	argsKey string
	obs     *observer
	// needInit: send init + full snapshot next tick. reset marks that
	// snapshot as a reset of data the client already holds.
	needInit bool
	reset    bool
	// needsReset: a snapshot is owed but held back while the connection is
	// coalescing (§8.1). Releasing it sets needInit; reset says whether the
	// client already holds data for the sub.
	needsReset bool
}

type subReq struct {
	c     *conn
	s     uint64
	name  string
	args  json.RawMessage
	have  bool
	ident *Identity
}

type unsubReq struct {
	c *conn
	s uint64
}

type closeReq struct{ c *conn }

// resumeReq unbinds every sub of c not in keep: the client unsubscribed
// while its unsub was lost in transit (§7.2). The client no longer holds
// those subs, so no end entry is sent.
type resumeReq struct {
	c    *conn
	keep map[uint64]bool
}

// rebindReq re-binds every sub of c to ident, and sends after once the
// resulting tx is enqueued (PROTOCOL.md §3.2).
type rebindReq struct {
	c     *conn
	ident *Identity
	after sequenced
}

func newEngine(a *App) *engine {
	return &engine{
		a:          a,
		fences:     map[string]*fence{},
		wake:       make(chan struct{}, 1),
		observers:  map[string]*observer{},
		conns:      map[*conn]map[uint64]*subscription{},
		ended:      map[*conn]map[uint64]bool{},
		coalescing: map[*conn]bool{},
	}
}

func (e *engine) signal() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

func (e *engine) enqueue(r any) {
	e.mu.Lock()
	e.reqs = append(e.reqs, r)
	e.mu.Unlock()
	e.signal()
}

func (e *engine) notify(n notification) {
	e.mu.Lock()
	e.nextN++
	n.n = e.nextN
	e.pending = append(e.pending, n)
	e.mu.Unlock()
	e.signal()
}

// registerFence must be called before the method's transaction commits, so
// the fence is known by the time its notification can be drained.
func (e *engine) registerFence(c *conn) (string, *fence) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fenceN++
	token := e.a.nodeID + ":" + strconv.FormatUint(e.fenceN, 10)
	f := &fence{released: make(chan struct{}), c: c}
	e.fences[token] = f
	return token, f
}

func (e *engine) dropFence(token string) {
	e.mu.Lock()
	delete(e.fences, token)
	e.mu.Unlock()
}

func (e *engine) run(ctx context.Context) {
	minTick := e.a.cfg.MinTickInterval
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.wake:
		}
		retry := e.tick(ctx)
		if retry {
			e.retryBackoff = min(max(e.retryBackoff*2, 250*time.Millisecond), 5*time.Second)
			time.AfterFunc(e.retryBackoff, e.signal)
		} else {
			e.retryBackoff = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(minTick):
		}
	}
}

type connOut struct {
	entries []subEntry
	after   []sequenced
}

// tick runs one engine round (PROTOCOL.md §0). It reports whether it must be
// retried because the snapshot could not be taken.
func (e *engine) tick(ctx context.Context) (retry bool) {
	// 1. Drain notifications (cutoff p) and requests, before the snapshot.
	e.mu.Lock()
	notes, reqs := e.pending, e.reqs
	e.pending, e.reqs = nil, nil
	fences, evts := e.carryFences, e.carryEvts
	e.carryFences, e.carryEvts = nil, nil
	for _, n := range notes {
		if n.evt != nil {
			evts = append(evts, n.evt)
		}
		if n.token != "" {
			if f := e.fences[n.token]; f != nil {
				delete(e.fences, n.token)
				fences = append(fences, f)
			}
		}
	}
	e.mu.Unlock()

	dirtyTables := map[string]bool{}
	allDirty := false
	for _, n := range notes {
		switch {
		case n.all:
			allDirty = true
		case n.table != "":
			dirtyTables[n.table] = true
		}
	}

	// Output held back by a failed tick goes out first, in order.
	out := e.carryOut
	e.carryOut = nil
	if out == nil {
		out = map[*conn]*connOut{}
	}
	outFor := func(c *conn) *connOut {
		o := out[c]
		if o == nil {
			o = &connOut{}
			out[c] = o
		}
		return o
	}

	for _, r := range reqs {
		e.handleReq(ctx, r, outFor)
	}

	// Coalescing connections (§8.1) get their held resets once their queue
	// has drained, and right away when this tick releases one of their
	// fences or sends them an after message: a res or authRes must follow a
	// tx reflecting current data for every sub (§3.2, §6.2). Both happen
	// before the snapshot, so the resets are built from it.
	forced := map[*conn]bool{}
	for _, f := range fences {
		if f.c != nil {
			forced[f.c] = true
		}
	}
	for c, o := range out {
		if len(o.after) > 0 {
			forced[c] = true
		}
	}
	for c := range e.coalescing {
		if forced[c] || c.queuedBytes(lowWaterBytes) < lowWaterBytes {
			e.release(c)
		}
	}

	for _, obs := range e.observers {
		if allDirty || slices.ContainsFunc(obs.tables, func(t string) bool { return dirtyTables[t] }) {
			obs.dirty = true
		}
	}

	// 2–3. One REPEATABLE READ snapshot for every observer that must run.
	// Observers with subs awaiting a snapshot also run, so a tx never pairs
	// a fresh snapshot with a cached result from an older one.
	var toRun []*observer
	for _, obs := range e.observers {
		if obs.dirty || obs.res == nil || obs.awaitsInit() {
			toRun = append(toRun, obs)
		}
	}
	if len(toRun) > 0 {
		if err := e.runObservers(ctx, toRun); err != nil {
			e.a.log.Warn("tick snapshot failed, retrying", "err", err)
			// Observers stay dirty; fences wait for a tick that actually
			// reflects their writes. Requests already applied stay applied.
			e.carryFences = fences
			e.carryEvts = evts
			e.carryOut = out
			return true
		}
	}

	// 4. Assemble one tx per connection. A connection over the soft limit
	// starts coalescing: its subs' changes are held, not serialized.
	// Decided once per connection, so the queue draining mid-assembly
	// cannot split one connection's subs between the two modes.
	decided := map[*conn]bool{}
	hold := func(c *conn) bool {
		if forced[c] {
			return false
		}
		if !decided[c] && !e.coalescing[c] && c.queuedBytes(0) > softQueuedBytes {
			e.coalescing[c] = true
			c.coalescing.Store(true)
			// Arm the drain signal; a queue that drained in the meantime
			// is picked up by the next tick.
			if c.queuedBytes(lowWaterBytes) < lowWaterBytes {
				e.signal()
			}
		}
		decided[c] = true
		return e.coalescing[c]
	}
	for _, obs := range e.observers {
		if obs.err != nil {
			for sub := range obs.subs {
				outFor(sub.c).entries = append(outFor(sub.c).entries, subEntry{S: sub.s, End: obs.err})
				delete(e.conns[sub.c], sub.s)
			}
			delete(e.observers, obs.key)
			continue
		}
		if obs.res == nil {
			continue // still pending (e.g. retried tick)
		}
		var opsJSON []json.RawMessage
		for sub := range obs.subs {
			switch {
			case sub.needInit && hold(sub.c):
				sub.needInit, sub.needsReset = false, true
			case sub.needInit:
				outFor(sub.c).entries = append(outFor(sub.c).entries, e.snapshotEntry(sub))
				sub.needInit, sub.reset = false, false
			case len(obs.ops) > 0 && hold(sub.c):
				if !sub.needsReset {
					sub.needsReset, sub.reset = true, true
				}
			case len(obs.ops) > 0:
				if opsJSON == nil {
					opsJSON = marshalOps(obs.ops)
				}
				outFor(sub.c).entries = append(outFor(sub.c).entries, subEntry{S: sub.s, Ops: opsJSON})
			}
		}
		obs.ops = nil
	}
	e.flush(out)

	// Events drained this tick follow the tx reflecting every change
	// notified before them, and precede the res of the call that emitted
	// them (its fence is notified after its events).
	e.deliverEvents(evts)

	// 5. Release fences satisfied by this tick: their writes were drained
	// before the snapshot, every dirty observer ran, and the resulting tx is
	// already enqueued ahead of the method's res.
	for _, f := range fences {
		close(f.released)
	}
	return false
}

func (e *engine) flush(out map[*conn]*connOut) {
	for c, o := range out {
		slices.SortStableFunc(o.entries, func(a, b subEntry) int { return cmp.Compare(a.S, b.S) })
		if c.resume {
			for _, en := range o.entries {
				if en.End != nil {
					if e.ended[c] == nil {
						e.ended[c] = map[uint64]bool{}
					}
					e.ended[c][en.S] = true
				}
			}
		}
		if len(o.entries) > 0 {
			c.sendTx(o.entries)
		}
		for _, m := range o.after {
			c.send(m)
		}
	}
}

// release ends coalescing for c: every held sub gets its snapshot in this
// tick, as a reset where the client already holds data for it. The snapshot
// is this tick's, newer than anything c was sent, so no sub goes back.
func (e *engine) release(c *conn) {
	delete(e.coalescing, c)
	c.coalescing.Store(false)
	for _, sub := range e.conns[c] {
		if sub.needsReset {
			sub.needsReset, sub.needInit = false, true
		}
	}
}

func (e *engine) snapshotEntry(sub *subscription) subEntry {
	obs := sub.obs
	init := &subInit{Key: obs.keyIdx, Ordered: !obs.pub.opts.Unordered}
	for _, c := range obs.cols {
		init.Cols = append(init.Cols, [2]string{c.name, c.code})
	}
	if len(obs.pub.tables) == 1 {
		init.Src = obs.pub.tables[0]
	}
	return subEntry{
		S:     sub.s,
		Init:  init,
		Reset: sub.reset,
		Ops:   marshalOps(snapshotOps(obs.res, init.Ordered)),
		Ready: true,
	}
}

func marshalOps(ops []op) []json.RawMessage {
	out := make([]json.RawMessage, len(ops))
	for i, o := range ops {
		out[i], _ = o.MarshalJSON()
	}
	return out
}

func (e *engine) handleReq(ctx context.Context, r any, outFor func(*conn) *connOut) {
	end := func(c *conn, s uint64, err *Error) {
		outFor(c).entries = append(outFor(c).entries, subEntry{S: s, End: err})
	}
	switch r := r.(type) {
	case subReq:
		if r.c.isClosed() || e.ended[r.c][r.s] {
			return
		}
		subs := e.conns[r.c]
		if subs == nil {
			subs = map[uint64]*subscription{}
			e.conns[r.c] = subs
		}
		argsKey, err := canonicalJSON(r.args)
		if err != nil {
			end(r.c, r.s, &Error{Code: CodeBadRequest, Msg: err.Error()})
			return
		}
		if old := subs[r.s]; old != nil {
			if old.name != r.name || old.argsKey != argsKey {
				end(r.c, r.s, NewError(CodeBadRequest, "sub id reused with different name or args"))
			}
			return // identical re-send is a no-op (§4.1)
		}
		if len(subs) >= e.a.cfg.MaxSubs {
			end(r.c, r.s, NewError(CodeRateLimited, "too many subscriptions"))
			return
		}
		sub := &subscription{c: r.c, s: r.s, name: r.name, args: r.args, argsKey: argsKey, needInit: true, reset: r.have}
		if err := e.bind(ctx, sub, r.ident); err != nil {
			end(r.c, r.s, err)
			return
		}
		subs[r.s] = sub

	case unsubReq:
		if sub := e.conns[r.c][r.s]; sub != nil {
			e.detach(sub)
			delete(e.conns[r.c], r.s)
		}

	case closeReq:
		for _, sub := range e.conns[r.c] {
			e.detach(sub)
		}
		delete(e.conns, r.c)
		delete(e.ended, r.c)
		delete(e.coalescing, r.c)

	case resumeReq:
		for s, sub := range e.conns[r.c] {
			if !r.keep[s] {
				e.detach(sub)
				delete(e.conns[r.c], s)
			}
		}

	case rebindReq:
		// v1-core: identity change resets every sub (§3.2), all in this tick's tx.
		for s, sub := range e.conns[r.c] {
			e.detach(sub)
			sub.needInit, sub.reset, sub.needsReset = true, true, false
			if err := e.bind(ctx, sub, r.ident); err != nil {
				end(r.c, s, err)
				delete(e.conns[r.c], s)
			}
		}
		outFor(r.c).after = append(outFor(r.c).after, r.after)
	}
}

// bind attaches sub to the observer for (pub, args, scope), creating it if needed.
func (e *engine) bind(ctx context.Context, sub *subscription, ident *Identity) *Error {
	pub := e.a.pubs[sub.name]
	if pub == nil {
		return NewError(CodeNotFound, "unknown publication "+sub.name)
	}
	key := observerKey(sub.name, sub.argsKey, scopeKey(pub, ident.userID()))
	obs := e.observers[key]
	if obs == nil {
		q, err := pub.build(&SubCtx{Context: ctx, Identity: ident}, sub.args)
		if err != nil {
			return e.a.toWireError(err, "publication "+sub.name)
		}
		obs = &observer{key: key, pub: pub, q: q, tables: pub.tables, subs: map[*subscription]struct{}{}}
		e.observers[key] = obs
	}
	obs.subs[sub] = struct{}{}
	sub.obs = obs
	return nil
}

func (e *engine) detach(sub *subscription) {
	obs := sub.obs
	if obs == nil {
		return
	}
	delete(obs.subs, sub)
	sub.obs = nil
	if len(obs.subs) == 0 {
		delete(e.observers, obs.key)
	}
}

// errTooLarge marks a result over the row cap.
var errTooLarge = errors.New("result too large")

type runResult struct {
	obs    *observer
	cols   []colSpec
	keyIdx []int
	res    *result
	err    *Error
}

// runObservers re-runs toRun inside one REPEATABLE READ snapshot. Results are
// staged and applied only once the snapshot transaction has committed, so a
// failure leaves every observer exactly as it was, still dirty.
func (e *engine) runObservers(ctx context.Context, toRun []*observer) error {
	tx, err := e.a.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", e.a.cfg.QueryTimeout.Milliseconds())); err != nil {
		return err
	}
	staged := make([]runResult, 0, len(toRun))
	for _, obs := range toRun {
		// A savepoint per observer keeps one failing query from poisoning
		// the snapshot for the rest.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		r := runResult{obs: obs}
		var qerr error
		r.cols, r.keyIdx, r.res, qerr = e.query(ctx, sp, obs)
		if qerr != nil {
			if err := sp.Rollback(ctx); err != nil {
				return err
			}
			r.err = e.queryError(obs, qerr)
		} else if err := sp.Commit(ctx); err != nil {
			return err
		}
		staged = append(staged, r)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	for _, r := range staged {
		obs := r.obs
		obs.dirty = false
		if r.err != nil {
			obs.err = r.err
			continue
		}
		switch {
		case obs.res == nil:
			// Fresh observer: subs get the snapshot via needInit.
		case !sameCols(obs.cols, r.cols):
			// Result shape changed (e.g. migration): reset every sub.
			for sub := range obs.subs {
				sub.needInit, sub.reset = true, true
			}
		default:
			obs.ops = diffResults(obs.res, r.res, !obs.pub.opts.Unordered)
		}
		obs.cols, obs.keyIdx, obs.res = r.cols, r.keyIdx, r.res
	}
	return nil
}

// sortedSubs returns obs's subs ordered by connection and sub id, so
// per-observer output has a stable order.
func (obs *observer) sortedSubs() []*subscription {
	subs := slices.Collect(maps.Keys(obs.subs))
	slices.SortFunc(subs, func(a, b *subscription) int {
		return cmp.Or(strings.Compare(a.c.session, b.c.session), cmp.Compare(a.s, b.s))
	})
	return subs
}

func (obs *observer) awaitsInit() bool {
	for sub := range obs.subs {
		if sub.needInit {
			return true
		}
	}
	return false
}

func (e *engine) queryError(obs *observer, err error) *Error {
	var pgErr *pgconn.PgError
	switch {
	case errors.Is(err, errTooLarge):
		return NewError(CodeResultTooLarge, fmt.Sprintf("result exceeds %d rows", e.a.cfg.MaxRows))
	case errors.As(err, &pgErr) && pgErr.Code == "57014":
		return NewError(CodeTimeout, "publication query timed out")
	}
	return e.a.toWireError(err, "publication "+obs.pub.name)
}

func (e *engine) query(ctx context.Context, tx pgx.Tx, obs *observer) ([]colSpec, []int, *result, error) {
	rows, err := tx.Query(ctx, obs.q.SQL, obs.q.Args...)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	m := tx.Conn().TypeMap()
	fds := rows.FieldDescriptions()
	cols := make([]colSpec, len(fds))
	for i, fd := range fds {
		cols[i] = colSpec{name: fd.Name, code: typeCode(m, fd.DataTypeOID), oid: fd.DataTypeOID}
	}
	keyIdx := make([]int, 0, len(obs.pub.opts.Key))
	for _, k := range obs.pub.opts.Key {
		i := slices.IndexFunc(cols, func(c colSpec) bool { return c.name == k })
		if i < 0 {
			return nil, nil, nil, fmt.Errorf("publication %s: key column %q not in result", obs.pub.name, k)
		}
		keyIdx = append(keyIdx, i)
	}
	res := &result{rows: map[string][]json.RawMessage{}}
	for rows.Next() {
		if len(res.keys) >= e.a.cfg.MaxRows {
			return nil, nil, nil, errTooLarge
		}
		vals, err := rows.Values()
		if err != nil {
			return nil, nil, nil, err
		}
		raws := rows.RawValues()
		cells := make([]json.RawMessage, len(cols))
		for i, c := range cols {
			if cells[i], err = encodeCell(m, c.code, c.oid, vals[i], raws[i], fds[i].Format); err != nil {
				return nil, nil, nil, fmt.Errorf("column %s: %w", c.name, err)
			}
		}
		key, err := rowKey(cells, keyIdx)
		if err != nil {
			return nil, nil, nil, err
		}
		if _, dup := res.rows[key]; dup {
			return nil, nil, nil, fmt.Errorf("publication %s: duplicate key %s; Key must be unique in the result", obs.pub.name, key)
		}
		res.keys = append(res.keys, key)
		res.rows[key] = cells
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}
	return cols, keyIdx, res, nil
}

func sameCols(a, b []colSpec) bool {
	return slices.EqualFunc(a, b, func(x, y colSpec) bool { return x.name == y.name && x.code == y.code })
}

// listen maintains the LISTEN connection (PROTOCOL.md §11).
func (e *engine) listen(ctx context.Context) {
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		err := e.listenOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		e.a.log.Warn("listener disconnected", "err", err, "retry", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

func (e *engine) listenOnce(ctx context.Context) error {
	conn, err := pgx.ConnectConfig(ctx, e.a.db.Config().ConnConfig.Copy())
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	for _, ch := range []string{"comet", evtChannel} {
		if _, err := conn.Exec(ctx, "LISTEN "+ch); err != nil {
			return err
		}
	}
	// Notifications sent while we were not listening are lost.
	e.notify(notification{all: true})
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if n.Channel == evtChannel {
			if evt, ok := parseEvt(n.Payload); ok {
				e.notify(notification{evt: evt})
			}
			continue
		}
		e.notify(parseNotification(n.Payload))
	}
}

// parseNotification parses a §11 payload. Anything unrecognised marks all
// observers dirty rather than risk a missed change.
func parseNotification(p string) notification {
	parts := strings.SplitN(p, "|", 5)
	if len(parts) >= 3 && parts[0] == "1" {
		switch parts[1] {
		case "r":
			if len(parts) == 5 {
				return notification{table: parts[3]}
			}
		case "t":
			if len(parts) == 4 {
				return notification{table: parts[3]}
			}
		case "f":
			return notification{token: strings.Join(parts[2:], "|")}
		}
	}
	return notification{all: true}
}

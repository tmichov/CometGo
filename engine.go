package cometgo

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// listenGen counts successful LISTENs (listening).
	listenGen uint64
	// listenMu orders fenced commits against LISTEN: commitFenced holds it
	// for reading, the listener for writing from LISTEN until its all-dirty
	// notification is queued. So a commit, together with its fence's mark,
	// lands wholly before or wholly after each LISTEN.
	listenMu sync.RWMutex

	// Owned by the run goroutine.
	observers map[string]*observer
	conns     map[*conn]map[uint64]*subscription
	// ended records, per resumable session, the sub ids whose end was sent.
	// After a resume the client re-sends its subs before the replayed end
	// reaches it; re-binding such an id would leave a sub the client never
	// tracks again. Sub ids are never reused (§4.1), so a sub for one of
	// these is always that re-send.
	ended map[*conn]map[uint64]bool
	// tables caches table metadata for changed-key filtering.
	tables map[string]*tableInfo
	// runs counts observer query executions (tests and benchmarks).
	runs atomic.Int64
	// coalescing holds the connections in coalescing mode (§8.1): their
	// queue went over softQueuedBytes, so their subs' changes are held as
	// needsReset until it drains below lowWaterBytes.
	coalescing map[*conn]bool
	// snapshotted maps the connections sent a snapshot since their queue
	// was last below lowWaterBytes to their queued bytes right after it.
	// Their soft limit is that much higher: a queue full of a snapshot is
	// not a client falling behind on diffs, and coalescing would answer it
	// with yet another snapshot.
	snapshotted map[*conn]int
	// pgTypes caches the wire code the catalog gives types pgx does not
	// know, by OID: "enum", "[]enum", or "" for the default (types.go).
	pgTypes      map[uint32]string
	carryFences  []*fence
	carryEvts    []*evtNote
	carryOut     map[*conn]*connOut
	retryBackoff time.Duration
}

type notification struct {
	n   uint64
	all bool // listener (re)connected or unparseable: everything is dirty
	// listen is the listener generation whose LISTEN just succeeded; 0 for
	// every other notification.
	listen uint64
	table  string // r/t notifications
	keys   string // r notifications: JSON array of PK arrays
	token  string // fence
	evt    *evtNote
}

type fence struct {
	released chan struct{}
	c        *conn // the connection whose res waits on it
	// committed is set once the method's transaction has committed, and gen
	// is listenGen at that moment (commitFenced).
	committed bool
	gen       uint64
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
	userID  string // the UserID it was last bound for
	// needInit: send init + full snapshot next tick. holds: the client
	// already has data for the sub (an earlier snapshot, or have on sub), so
	// that snapshot goes out as a reset.
	needInit bool
	holds    bool
	// needsReset: a snapshot is owed but held back while the connection is
	// coalescing (§8.1). Releasing it sets needInit.
	needsReset bool
}

type subReq struct {
	c    *conn
	s    uint64
	name string
	args json.RawMessage
	have bool
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

// revalidateReq re-runs fn for every sub of every session whose identity
// has userID, ending those it now rejects.
type revalidateReq struct{ userID string }

func newEngine(a *App) *engine {
	return &engine{
		a:           a,
		fences:      map[string]*fence{},
		wake:        make(chan struct{}, 1),
		observers:   map[string]*observer{},
		conns:       map[*conn]map[uint64]*subscription{},
		ended:       map[*conn]map[uint64]bool{},
		tables:      map[string]*tableInfo{},
		coalescing:  map[*conn]bool{},
		snapshotted: map[*conn]int{},
		pgTypes:     map[uint32]string{},
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

// commitFenced commits a method's transaction whose last statement
// notified token's fence, and marks the fence committed. Postgres drops
// notifications for a channel nobody listens on, so the fence of a method
// that committed while the listener was down never arrives; the all-dirty
// tick of the next successful LISTEN releases it instead (tick).
func (e *engine) commitFenced(ctx context.Context, tx pgx.Tx, token string) error {
	e.listenMu.RLock()
	defer e.listenMu.RUnlock()
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	e.markCommitted(token)
	return nil
}

func (e *engine) markCommitted(token string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if f := e.fences[token]; f != nil { // nil: already released
		f.committed, f.gen = true, e.listenGen
	}
}

// listening records a successful LISTEN, with listenMu held for writing,
// and marks everything dirty: notifications sent while nobody listened are
// lost.
func (e *engine) listening() {
	e.mu.Lock()
	e.listenGen++
	e.nextN++
	e.pending = append(e.pending, notification{n: e.nextN, all: true, listen: e.listenGen})
	e.mu.Unlock()
	e.signal()
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
	allDirty := false
	var listened uint64
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
		allDirty = allDirty || n.all
		listened = max(listened, n.listen)
	}
	// A fence committed before listener generation `listened` issued LISTEN
	// (listenMu) never reaches that connection: its notifications were
	// either received earlier, and so are in this drain or an earlier one,
	// or lost. This all-dirty tick re-runs every observer on a snapshot
	// taken after this drain, so it covers the fence's writes, and the
	// fence's events, if received, go out before its res (step 5). A fence
	// committed later is left to its own notification: releasing it here
	// could send its res ahead of its events.
	if listened > 0 {
		for token, f := range e.fences {
			if f.committed && f.gen < listened {
				delete(e.fences, token)
				fences = append(fences, f)
			}
		}
	}
	e.mu.Unlock()

	changes := map[string]*tableChange{}
	for _, n := range notes {
		if n.table != "" {
			tc := changes[n.table]
			if tc == nil {
				tc = &tableChange{}
				changes[n.table] = tc
			}
			tc.add(n)
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
	// A session without a live socket stays held: releasing it would put
	// every held snapshot into the replay buffer, which can end the session.
	// After a resume the next tick checks the new socket.
	for c := range e.coalescing {
		if q, live := c.queuedBytes(lowWaterBytes); forced[c] || live && q < lowWaterBytes {
			e.release(c)
		}
	}
	for c := range e.snapshotted {
		if q, _ := c.queuedBytes(lowWaterBytes); q < lowWaterBytes {
			delete(e.snapshotted, c)
		}
	}

	// Observers whose query declares Match and whose table only reported
	// changed keys are candidates: they run only if those keys can affect
	// them, decided inside the snapshot (pkfilter.go).
	var cands []*observer
	for _, obs := range e.observers {
		if !allDirty && !slices.ContainsFunc(obs.tables, func(t string) bool { return changes[t] != nil }) {
			continue
		}
		if !allDirty && !obs.dirty && !obs.awaitsInit() && obs.filterable(changes[obs.tables[0]]) {
			cands = append(cands, obs)
		} else {
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
	if len(toRun) > 0 || len(cands) > 0 {
		if err := e.runObservers(ctx, toRun, cands, changes); err != nil {
			e.a.log.Warn("tick snapshot failed, retrying", "err", err)
			// Observers stay dirty; fences wait for a tick that actually
			// reflects their writes. Requests already applied stay applied.
			// Candidates lose the keys that made them candidates, so they
			// re-run in full.
			for _, obs := range cands {
				obs.dirty = true
			}
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
		if !decided[c] && !e.coalescing[c] {
			if q, _ := c.queuedBytes(0); q > softQueuedBytes+e.snapshotted[c] {
				e.coalescing[c] = true
				c.coalescing.Store(true)
				// Arm the drain signal; a queue that drained in the
				// meantime is picked up by the next tick.
				if q, _ := c.queuedBytes(lowWaterBytes); q < lowWaterBytes {
					e.signal()
				}
			}
		}
		decided[c] = true
		return e.coalescing[c]
	}
	snapped := map[*conn]bool{}
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
				sub.needInit, sub.holds = false, true
				snapped[sub.c] = true
			case len(obs.ops) > 0 && hold(sub.c):
				sub.needsReset = true
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
	for c := range snapped {
		// Arms the drain signal that ends the raised limit.
		q, _ := c.queuedBytes(lowWaterBytes)
		e.snapshotted[c] = q
	}

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
// tick, as a reset where the client holds data for it. The snapshot
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
		Reset: sub.holds,
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
		sub := &subscription{c: r.c, s: r.s, name: r.name, args: r.args, argsKey: argsKey, needInit: true, holds: r.have}
		// The identity is read here, not when the sub arrived: an auth
		// change sets it before queuing its rebind, so a sub handled
		// before that rebind is rebound by it, and one handled after it
		// already sees the new identity.
		if err := e.bind(ctx, sub, r.c.identity()); err != nil {
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
		delete(e.snapshotted, r.c)

	case resumeReq:
		for s, sub := range e.conns[r.c] {
			if !r.keep[s] {
				e.detach(sub)
				delete(e.conns[r.c], s)
			}
		}

	case revalidateReq:
		// Not a rebind: the observers stay as they are; fn only re-checks
		// access, so its Query is discarded.
		for c, subs := range e.conns {
			// A session whose identity changed since its subs were bound
			// has a rebind queued, which re-checks them anyway.
			ident := c.identity()
			if c.isClosed() || ident.userID() != r.userID {
				continue
			}
			for s, sub := range subs {
				if sub.userID != r.userID {
					continue
				}
				if err := e.recheck(ctx, sub, ident); err != nil {
					e.detach(sub)
					delete(subs, s)
					end(c, s, err)
				}
			}
		}

	case rebindReq:
		// v1-core: identity change resets every sub (§3.2), all in this tick's tx.
		for s, sub := range e.conns[r.c] {
			e.detach(sub)
			sub.needInit, sub.needsReset = true, false
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
	sub.obs, sub.userID = obs, ident.userID()
	return nil
}

// recheck runs sub's fn for ident. A panic, which would otherwise take down
// the engine goroutine, becomes INTERNAL.
func (e *engine) recheck(ctx context.Context, sub *subscription, ident *Identity) *Error {
	pub := e.a.pubs[sub.name]
	_, err := func() (q Query, err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
			}
		}()
		return pub.build(&SubCtx{Context: ctx, Identity: ident}, sub.args)
	}()
	if err != nil {
		return e.a.toWireError(err, "publication "+sub.name)
	}
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

// runObservers re-runs toRun inside one REPEATABLE READ snapshot, plus each
// candidate the changed rows can affect. Results are staged and applied only
// once the snapshot transaction has committed, so a failure leaves every
// observer exactly as it was.
func (e *engine) runObservers(ctx context.Context, toRun, cands []*observer, changes map[string]*tableChange) error {
	tx, err := e.a.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", e.a.cfg.QueryTimeout.Milliseconds())); err != nil {
		return err
	}
	if len(cands) > 0 {
		affected, err := e.filterCandidates(ctx, tx, cands, changes)
		if err != nil {
			return err
		}
		toRun = append(toRun, affected...)
	}
	staged := make([]runResult, 0, len(toRun))
	for _, obs := range toRun {
		e.runs.Add(1)
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
				sub.needInit = true
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
	// Types pgx does not know, such as user enums: ask the catalog, once
	// per type, before the row loop.
	if err := e.resolveTypes(ctx, m, fds); err != nil {
		return nil, nil, nil, err
	}
	cols := make([]colSpec, len(fds))
	for i, fd := range fds {
		cols[i] = colSpec{name: fd.Name, code: e.colCode(m, fd.DataTypeOID), oid: fd.DataTypeOID}
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
	// Fenced commits wait while LISTEN runs (listenMu).
	e.listenMu.Lock()
	for _, ch := range []string{"comet", evtChannel, revalChannel} {
		if _, err := conn.Exec(ctx, "LISTEN "+ch); err != nil {
			e.listenMu.Unlock()
			return err
		}
	}
	// Notifications sent while we were not listening are lost.
	e.listening()
	e.listenMu.Unlock()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return err
		}
		switch n.Channel {
		case evtChannel:
			if evt, ok := parseEvt(n.Payload); ok {
				e.notify(notification{evt: evt})
			}
			continue
		case revalChannel:
			// Enqueued before a fence notified later on this connection, so
			// the re-check runs no later than the tick releasing the fence.
			if n.Payload != "" {
				e.enqueue(revalidateReq{userID: n.Payload})
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
				return notification{table: parts[3], keys: parts[4]}
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

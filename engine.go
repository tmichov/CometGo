package cometgo

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
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
	// After listenIdle without a notification, the listener checks its
	// connection with a query that must answer within listenProbe.
	listenIdle, listenProbe time.Duration
	// Bounds each listener connect when the DSN sets no connect_timeout.
	listenConnect time.Duration

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
	pgTypes map[uint32]string
	// tickN numbers ticks; an observer's resTick is the tick of its result.
	tickN uint64
	// afters holds, per connection, the after messages of its rebinds in
	// order, until the connection is settled (§3.2).
	afters map[*conn][]pendingAfter
	// heldEvts are the observers with events waiting for their data.
	heldEvts     map[*observer]bool
	carryEvts    []heldEvt
	carryOut     map[*conn]*connOut
	retryBackoff time.Duration
	// deferObs, set by tests, keeps the observers it returns true for out
	// of a tick's run list, as the tick budget will (M3).
	deferObs func(*observer) bool
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
	reval  string // comet_reval: the user ID to re-check
}

type fence struct {
	token    string
	released chan struct{}
	c        *conn // the connection whose res waits on it
	// p is the number of the notification that drained the fence (its own,
	// or a reconnect's all-dirty one); 0 until then. Engine mutex.
	p uint64
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

	// staleFrom is the smallest drained notification number its delivered
	// result does not reflect; 0 when fresh. dirtySince is the same for
	// notifications drained while a run is in flight, after that run's
	// cutoff; it becomes staleFrom when the run completes. Runs never
	// outlive their tick yet, so it stays 0 until detach (M3). resTick is
	// the tick whose snapshot produced res.
	staleFrom  uint64
	dirtySince uint64
	resTick    uint64
	// dueFrom orders a due observer that is not stale (a new one, or one
	// owing a snapshot): the drain cutoff + 1 of the tick it first became
	// due in; 0 once it runs.
	dueFrom uint64
	// inFlight: its query outlives the tick that started it (detach, M3).
	// Never set yet. Such an observer is in no tick's run list.
	inFlight bool
	// evts are events waiting for this observer to reflect their n.
	evts []heldEvt

	// Per-tick output.
	ops      []op
	snapJSON []json.RawMessage // snapshot ops, built on the first snapshotEntry
	err      *Error
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

// revalidateReq re-runs fn for every sub of every session whose identity's
// UserID is in users (every session, if all), ending those it now rejects.
// tick merges a drain's comet_reval notifications into one, so each sub is
// re-checked at most once per tick however many Revalidate calls named its
// user.
type revalidateReq struct {
	users map[string]bool
	all   bool
}

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
		afters:      map[*conn][]pendingAfter{},
		heldEvts:    map[*observer]bool{},
		listenIdle:  30 * time.Second,
		listenProbe: 5 * time.Second,
		// Under the OS TCP connect timeout (about 2 min on Linux), so a
		// path that is still dead is retried soon after it comes back.
		listenConnect: 10 * time.Second,
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
	f := &fence{token: token, released: make(chan struct{}), c: c}
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
		var start time.Time
		var runs int64
		if e.a.cfg.Metrics != nil {
			start, runs = time.Now(), e.runs.Load()
		}
		retry := e.tick(ctx)
		if e.a.cfg.Metrics != nil {
			e.tickMetrics(start, e.runs.Load()-runs)
		}
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
}

// pendingAfter is a message sent once its connection is settled as of p
// (settled), such as a rebind's authRes (§3.2).
type pendingAfter struct {
	p uint64
	m sequenced
}

// heldEvt is an event with its notification number: it goes out once its
// observer reflects every change notified before it.
type heldEvt struct {
	n    uint64
	note *evtNote
}

// tick runs one engine round (PROTOCOL.md §0). It reports whether it must be
// retried because the snapshot could not be taken.
func (e *engine) tick(ctx context.Context) (retry bool) {
	e.tickN++
	// 1. Drain notifications (cutoff p) and requests, before the snapshot.
	e.mu.Lock()
	notes, reqs := e.pending, e.reqs
	e.pending, e.reqs = nil, nil
	cutoff := e.nextN
	evts := e.carryEvts
	e.carryEvts = nil
	reval := revalidateReq{users: map[string]bool{}}
	var listened, listenN, allN uint64
	for _, n := range notes {
		if n.evt != nil {
			evts = append(evts, heldEvt{n: n.n, note: n.evt})
		}
		if n.reval != "" {
			reval.users[n.reval] = true
		}
		if n.token != "" {
			if f := e.fences[n.token]; f != nil && f.p == 0 {
				f.p = n.n
			}
		}
		if n.all && allN == 0 {
			allN = n.n
		}
		if n.listen > listened {
			listened, listenN = n.listen, n.n
		}
	}
	// A fence committed before listener generation `listened` issued LISTEN
	// (listenMu) never reaches that connection: its notifications were
	// either received earlier, and so are in this drain or an earlier one,
	// or lost. The all-dirty notification of that LISTEN makes every
	// observer stale from listenN, so the fence is released once they
	// reflect it, and the fence's events, if received, go out before its
	// res. A fence committed later is left to its own notification:
	// releasing it here could send its res ahead of its events.
	if listened > 0 {
		for _, f := range e.fences {
			if f.committed && f.gen < listened && f.p == 0 {
				f.p = listenN
			}
		}
	}
	// Drained fences stay registered until released, so FenceTimeout's
	// dropFence takes a carried one out too.
	var fences []*fence
	for _, f := range e.fences {
		if f.p != 0 {
			fences = append(fences, f)
		}
	}
	e.mu.Unlock()

	changes := map[string]*tableChange{}
	for _, n := range notes {
		if n.table != "" {
			tc := changes[n.table]
			if tc == nil {
				tc = &tableChange{n: n.n}
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

	// Re-checks (comet_reval) sent while nobody listened are lost too, so
	// a tick after a reconnect re-checks every sub, as it re-runs every
	// observer. They run after the other requests, once each, when every sub
	// is bound to its connection's current identity, and before fences are
	// released, so an end still precedes the res of the method that caused
	// it.
	reval.all = listened > 0
	for _, r := range reqs {
		e.handleReq(ctx, r, outFor, cutoff)
	}
	if reval.all || len(reval.users) > 0 {
		e.handleReq(ctx, reval, outFor, cutoff)
	}

	deferred := map[*observer]bool{}
	isDeferred := func(obs *observer) bool {
		if e.deferObs == nil {
			return false
		}
		d, ok := deferred[obs]
		if !ok {
			d = e.deferObs(obs)
			deferred[obs] = d
		}
		return d
	}

	// Observers on a changed table become stale from the first notification
	// that changed it. Those whose query declares Match and whose table only
	// reported changed keys are candidates: they run only if those keys can
	// affect them, decided inside the snapshot (pkfilter.go).
	var cands []*observer
	for _, obs := range e.observers {
		from := allN
		for _, t := range obs.tables {
			if tc := changes[t]; tc != nil && (from == 0 || tc.n < from) {
				from = tc.n
			}
		}
		if from == 0 {
			continue
		}
		if obs.inFlight {
			// Its run's cutoff is past: it re-runs once that run completes.
			obs.dirty = true
			if obs.dirtySince == 0 || from < obs.dirtySince {
				obs.dirtySince = from
			}
			continue
		}
		obs.markStale(from)
		if allN == 0 && !obs.dirty && !obs.awaitsInit() && obs.filterable(changes[obs.tables[0]]) && !isDeferred(obs) {
			cands = append(cands, obs)
		} else {
			obs.dirty = true
		}
	}

	// Coalescing connections (§8.1) get their held resets once their queue
	// has drained, and right away when one of their fences or after
	// messages is expected to be released this tick: a res or authRes must
	// follow a tx reflecting current data for every sub (§3.2, §6.2). That
	// is expected when every stale observer of the connection runs this
	// tick. The resets are built from this tick's snapshot; a held sub whose
	// observer does not run stays held.
	forced := map[*conn]bool{}
	expected := func(c *conn) bool {
		for _, sub := range e.conns[c] {
			if !sub.obs.freshPast(cutoff) && isDeferred(sub.obs) {
				return false
			}
		}
		return true
	}
	for _, f := range fences {
		if f.c != nil && !forced[f.c] && expected(f.c) {
			forced[f.c] = true
		}
	}
	for c := range e.afters {
		if !forced[c] && expected(c) {
			forced[c] = true
		}
	}
	// A session without a live socket stays held: releasing it would put
	// every held snapshot into the replay buffer, which can end the session.
	// After a resume the next tick checks the new socket.
	releasing := map[*conn]bool{}
	resetFor := map[*observer]bool{}
	for c := range e.coalescing {
		if q, live := c.queuedBytes(lowWaterBytes); forced[c] || live && q < lowWaterBytes {
			releasing[c] = true
			for _, sub := range e.conns[c] {
				if sub.needsReset {
					resetFor[sub.obs] = true
				}
			}
		}
	}
	for c := range e.snapshotted {
		if q, _ := c.queuedBytes(lowWaterBytes); q < lowWaterBytes {
			delete(e.snapshotted, c)
		}
	}

	// 2–3. One REPEATABLE READ snapshot for every observer that must run.
	// Observers with subs awaiting a snapshot also run, so a tx never pairs
	// a fresh snapshot with a cached result from an older one (assembly
	// emits only results of this tick). A candidate owing a reset runs in
	// full. An observer in flight is never in the run list: it runs again
	// once it completes. runObservers starts them oldest first, within the
	// tick budget.
	var toRun []*observer
	for _, obs := range e.observers {
		if resetFor[obs] && !obs.dirty && slices.Contains(cands, obs) {
			cands = slices.DeleteFunc(cands, func(o *observer) bool { return o == obs })
			obs.dirty = true
		}
		if (obs.dirty || obs.res == nil || obs.awaitsInit() || resetFor[obs]) && !obs.inFlight && !isDeferred(obs) {
			if obs.staleFrom == 0 && obs.dueFrom == 0 {
				obs.dueFrom = cutoff + 1
			}
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
			e.carryEvts = evts
			e.carryOut = out
			return true
		}
	}
	for c := range releasing {
		e.release(c)
	}

	// 4. Assemble one tx per connection. A connection over the soft limit
	// starts coalescing: its subs' changes are held, not serialized.
	// Decided once per connection, so the queue draining mid-assembly
	// cannot split one connection's subs between the two modes. A
	// connection forced or released this tick holds nothing, even while it
	// stays coalescing for a sub whose observer did not run.
	decided := map[*conn]bool{}
	hold := func(c *conn) bool {
		if forced[c] || releasing[c] {
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
		// Only results of this tick's snapshot go out (§6.1.1). Subs of
		// an observer that did not run keep waiting.
		if obs.res == nil || obs.resTick != e.tickN {
			continue
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
		obs.ops, obs.snapJSON = nil, nil
	}
	e.flush(out)
	for c := range snapped {
		// Arms the drain signal that ends the raised limit.
		q, _ := c.queuedBytes(lowWaterBytes)
		e.snapshotted[c] = q
	}

	// After messages go out in order, each once its connection is settled.
	for c, q := range e.afters {
		for len(q) > 0 && e.settled(c, q[0].p) {
			c.send(q[0].m)
			q = q[1:]
		}
		if len(q) == 0 {
			delete(e.afters, c)
		} else {
			e.afters[c] = q
		}
	}

	// Events go out once their observer reflects every change notified
	// before them, and precede the res of the call that emitted them (its
	// fence is notified after its events).
	e.deliverEvents(evts)

	// 5. Release each fence whose connection is settled as of its
	// notification: every sub reflects its writes, and the tx carrying them
	// is already enqueued ahead of the method's res. The others carry.
	for _, f := range fences {
		if e.settled(f.c, f.p) {
			e.releaseFence(f)
		}
	}
	return false
}

// settled reports whether every sub of c has had its snapshot sent and has
// an observer that reflects every notification up to p (§6.2).
func (e *engine) settled(c *conn, p uint64) bool {
	for _, sub := range e.conns[c] {
		if sub.needInit || sub.needsReset || !sub.obs.freshPast(p) {
			return false
		}
	}
	return true
}

func (e *engine) releaseFence(f *fence) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fences[f.token] == f { // not dropped by FenceTimeout
		delete(e.fences, f.token)
		close(f.released)
	}
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
	}
}

// release ends coalescing for c as far as this tick's snapshot allows:
// every held sub whose observer ran gets its snapshot in this tick, as a
// reset where the client holds data for it. The snapshot is this tick's,
// newer than anything c was sent, so no sub goes back. c stays coalescing
// while a sub's observer did not run.
func (e *engine) release(c *conn) {
	held := false
	for _, sub := range e.conns[c] {
		if !sub.needsReset {
			continue
		}
		if sub.obs.resTick == e.tickN {
			sub.needsReset, sub.needInit = false, true
		} else {
			held = true
		}
	}
	if !held {
		delete(e.coalescing, c)
		c.coalescing.Store(false)
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
	// Every sub of obs that needs a snapshot this tick shares one encoding.
	if obs.snapJSON == nil {
		obs.snapJSON = marshalOps(snapshotOps(obs.res, init.Ordered))
	}
	return subEntry{
		S:     sub.s,
		Init:  init,
		Reset: sub.holds,
		Ops:   obs.snapJSON,
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

// handleReq applies one request. cutoff is the tick's drain cutoff: a
// rebind's after message waits until the connection reflects it.
func (e *engine) handleReq(ctx context.Context, r any, outFor func(*conn) *connOut, cutoff uint64) {
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
		delete(e.afters, r.c)

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
			if c.isClosed() || (!r.all && !r.users[ident.userID()]) {
				continue
			}
			for s, sub := range subs {
				if sub.userID != ident.userID() {
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
		// Every sub is detached before any rebinds, so observers only the old
		// identity held are freed before MaxObservers is checked.
		for _, sub := range e.conns[r.c] {
			e.detach(sub)
			sub.needInit, sub.needsReset = true, false
		}
		for s, sub := range e.conns[r.c] {
			if err := e.bind(ctx, sub, r.ident); err != nil {
				end(r.c, s, err)
				delete(e.conns[r.c], s)
			}
		}
		e.afters[r.c] = append(e.afters[r.c], pendingAfter{p: cutoff, m: r.after})
	}
}

// bind attaches sub to the observer for (pub, args, scope), creating it if needed.
func (e *engine) bind(ctx context.Context, sub *subscription, ident *Identity) *Error {
	pub := e.a.pubs[sub.name]
	if pub == nil {
		return NewError(CodeNotFound, "unknown publication "+sub.name)
	}
	key := observerKey(sub.name, sub.argsKey, scopeKey(pub, ident.Scope()))
	// fn runs for every sub, not only the observer's first: it may check
	// access for this caller. The key already determines the query, so a
	// joining sub's Query is discarded. fn runs on the engine goroutine, so
	// a panic would take down the process; end only this sub instead.
	q, err := func() (q Query, err error) {
		defer recoverErr(&err)
		return pub.build(&SubCtx{Context: ctx, Identity: ident}, sub.args)
	}()
	if err != nil {
		return e.a.toWireError(err, "publication "+sub.name)
	}
	obs := e.observers[key]
	if obs == nil {
		// Checked after fn, so its FORBIDDEN or VALIDATION wins over a
		// retryable OVERLOADED.
		if e.a.cfg.MaxObservers > 0 && len(e.observers) >= e.a.cfg.MaxObservers {
			return NewError(CodeOverloaded, "too many live queries on the server")
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
		defer recoverErr(&err)
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
	took   time.Duration // query time, measured only for Config.Metrics
}

// runObservers re-runs toRun inside one REPEATABLE READ snapshot, plus each
// candidate the changed rows can affect. Results are staged and applied only
// once the snapshot transaction has committed, so a failure leaves every
// observer exactly as it was.
func (e *engine) runObservers(ctx context.Context, toRun, cands []*observer, changes map[string]*tableChange) (err error) {
	// Pool hooks and tracers (BeforeAcquire, PrepareConn, ConnConfig.Tracer)
	// run user code in BeginTx, the SET LOCAL and the candidate filter. A
	// panic there fails the tick, which tick retries like a lost snapshot.
	// The per-observer recover below still isolates one observer's query.
	// This also catches a panic in the framework's own apply loop after the
	// commit (diffResults): deliberately, since crashing would only restart
	// into the same data and take methods down too. A deterministic bug
	// there stalls live updates, retried with backoff and logged with its
	// stack each time, while methods keep answering (with lag). Observers
	// applied before the panic keep their ops for the next good tick.
	defer recoverErr(&err)
	// Acquired explicitly so a panic during BEGIN releases the connection
	// (pgxpool's BeginTx would leak it).
	pc, err := e.a.db.Acquire(ctx)
	if err != nil {
		return err
	}
	defer pc.Release()
	tx, err := pc.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", e.a.cfg.QueryTimeout.Milliseconds())); err != nil {
		return err
	}
	snapStart := time.Now()
	var affected []*observer
	if len(cands) > 0 {
		if affected, err = e.filterCandidates(ctx, tx, cands, changes); err != nil {
			return err
		}
		toRun = append(toRun, affected...)
	}
	// Oldest first, so observers a budget left unstarted go first next
	// tick and none starves under constant writes.
	slices.SortStableFunc(toRun, func(a, b *observer) int { return cmp.Compare(a.age(), b.age()) })
	budget := 2 * e.a.cfg.SlowObserverAfter
	staged := make([]runResult, 0, len(toRun))
	for i, obs := range toRun {
		if i > 0 && time.Since(snapStart) >= budget {
			// The rest stay due (dirty, stale or awaiting init) and their
			// connections' fences carry. An affected candidate becomes dirty.
			// Nothing else may wake the engine for them, so tick again.
			for _, obs := range toRun[i:] {
				obs.dirty = true
			}
			e.signal()
			break
		}
		e.runs.Add(1)
		// A savepoint per observer keeps one failing query from poisoning
		// the snapshot for the rest.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		r := runResult{obs: obs}
		var qerr error
		var start time.Time
		if e.a.cfg.Metrics != nil {
			start = time.Now()
		}
		func() {
			// Encoding Query.Args runs user code (driver.Valuer, pgx
			// encoders) on the engine goroutine; a panic there fails only
			// this observer. pgx encodes before sending, so sp is still
			// usable for the rollback. A panic after sending (decoding
			// rows) is caught too; rows.Close drains the result first, and
			// if the rollback fails anyway the tick retries.
			defer recoverErr(&qerr)
			r.cols, r.keyIdx, r.res, qerr = e.query(ctx, sp, obs)
		}()
		if e.a.cfg.Metrics != nil {
			r.took = time.Since(start)
		}
		if errors.Is(qerr, errTypeLookup) {
			// Not this observer's fault (e.g. the pool is exhausted): retry
			// the tick rather than end its subs.
			return qerr
		}
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
	// Applied only once the snapshot commits: a failed tick leaves them
	// stale and dirty.
	ran := make(map[*observer]bool, len(affected))
	for _, obs := range affected {
		ran[obs] = true
	}
	for _, obs := range cands {
		if !ran[obs] {
			obs.staleFrom = 0 // fresh as of this snapshot
		}
	}
	if e.a.cfg.Metrics != nil {
		// Reported after the commit: runs of a failed snapshot are retried
		// and would be counted twice.
		for _, r := range staged {
			e.a.metric(MetricPubRun, r.obs.pub.name, r.took.Seconds())
		}
	}
	for _, r := range staged {
		obs := r.obs
		obs.dirty = false
		obs.staleFrom, obs.dirtySince, obs.resTick, obs.dueFrom = obs.dirtySince, 0, e.tickN, 0
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
			var reset bool
			obs.ops, reset = diffResults(obs.res, r.res, !obs.pub.opts.Unordered)
			if reset {
				// Most rows moved: a snapshot is cheaper than the moves.
				for sub := range obs.subs {
					sub.needInit = true
				}
			}
		}
		obs.cols, obs.keyIdx, obs.res = r.cols, r.keyIdx, r.res
	}
	return nil
}

// age orders due observers: the first notification it does not reflect,
// or the cutoff it became due at, whichever is older.
func (obs *observer) age() uint64 {
	if obs.staleFrom != 0 && (obs.dueFrom == 0 || obs.staleFrom < obs.dueFrom) {
		return obs.staleFrom
	}
	return obs.dueFrom
}

// markStale records that obs does not reflect notification n.
func (obs *observer) markStale(n uint64) {
	if obs.staleFrom == 0 || n < obs.staleFrom {
		obs.staleFrom = n
	}
}

// freshPast reports whether obs's result reflects every notification up
// to p. An observer with no result reflects nothing.
func (obs *observer) freshPast(p uint64) bool {
	return obs.res != nil && (obs.staleFrom == 0 || obs.staleFrom > p)
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
	// Every gap delays the calls committed in it (they wait for the
	// reconnect tick), so maxBackoff stays well under the default 5s
	// FenceTimeout. A connection that stayed up for stableAfter resets the
	// backoff; one that drops sooner keeps backing off, so a connection
	// cut right after LISTEN cannot trigger an all-dirty re-run every
	// 500ms.
	const (
		minBackoff  = 500 * time.Millisecond
		maxBackoff  = 2 * time.Second
		stableAfter = 30 * time.Second
	)
	backoff := minBackoff
	for ctx.Err() == nil {
		listenedAt, err := e.listenOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if !listenedAt.IsZero() && time.Since(listenedAt) >= stableAfter {
			backoff = minBackoff
		}
		e.a.log.Warn("listener disconnected", "err", err, "retry", backoff)
		e.a.metric(MetricListenReconnect, "", 1)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// listenOnce runs one listener connection until it fails. It reports when
// LISTEN succeeded, or the zero time if it did not.
func (e *engine) listenOnce(ctx context.Context) (listenedAt time.Time, err error) {
	// The connection copies the pool's ConnConfig, tracer included, so user
	// code runs here too. A panic in it drops the connection like a network
	// error, and run reconnects.
	defer recoverErr(&err)
	cfg := e.a.db.Config().ConnConfig.Copy()
	// An idle wait that times out must leave the connection usable for the
	// probe: interrupt it with a read deadline, never a cancel request, which
	// could cancel the probe instead.
	cfg.BuildContextWatcherHandler = func(c *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.DeadlineContextWatcherHandler{Conn: c.Conn()}
	}
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = e.listenConnect
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return time.Time{}, err
	}
	defer conn.Close(context.Background())
	// Fenced commits wait while LISTEN runs (listenMu). Unlocked by defer, so
	// a panic cannot leave it held.
	err = func() error {
		e.listenMu.Lock()
		defer e.listenMu.Unlock()
		for _, ch := range []string{"comet", evtChannel, revalChannel} {
			if _, err := conn.Exec(ctx, "LISTEN "+ch); err != nil {
				return err
			}
		}
		// Notifications sent while we were not listening are lost.
		e.listening()
		return nil
	}()
	if err != nil {
		return time.Time{}, err
	}
	listenedAt = time.Now()
	for {
		waitCtx, cancel := context.WithTimeout(ctx, e.listenIdle)
		n, err := conn.WaitForNotification(waitCtx)
		idle := waitCtx.Err() != nil && ctx.Err() == nil
		cancel()
		if err != nil && idle {
			// Quiet for listenIdle: check the connection still answers. A path
			// that died without a TCP reset (failover, black hole) would go
			// unnoticed for minutes otherwise, with every subscription stale.
			// Notifications arriving meanwhile are buffered by pgx.
			probeCtx, cancel := context.WithTimeout(ctx, e.listenProbe)
			_, err = conn.Exec(probeCtx, "SELECT 1")
			cancel()
			if err != nil {
				return listenedAt, fmt.Errorf("listener probe: %w", err)
			}
			continue
		}
		if err != nil {
			return listenedAt, err
		}
		switch n.Channel {
		case evtChannel:
			if evt, ok := parseEvt(n.Payload); ok {
				e.notify(notification{evt: evt})
			}
			continue
		case revalChannel:
			// Numbered before a fence notified later on this connection, so
			// the re-check runs no later than the tick releasing the fence.
			if n.Payload != "" {
				e.notify(notification{reval: n.Payload})
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

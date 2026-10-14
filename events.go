package cometgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// capEvt is the hello/welcome cap for ephemeral events. The server sends evt
// messages only to clients that offered it.
const capEvt = "evt"

// evtChannel is the Postgres channel events travel on, so an emit on any node
// reaches subscribers on every node listening to the same database.
const evtChannel = "comet_evt"

// maxEvtPayload keeps an event's notification under Postgres' 8000-byte
// pg_notify limit.
const maxEvtPayload = 7500

// ErrEventTooLarge is returned by Emit and EmitAfterCommit when the encoded
// event exceeds the 7,500-byte notification limit. Nothing is sent.
var ErrEventTooLarge = errors.New("cometgo: event too large")

// ErrUserIDTooLong is returned by Revalidate and RevalidateAfterCommit for a
// userID of 8,000 bytes or more, which a Postgres notification cannot carry.
// Nothing is sent, and the method's transaction is not touched.
var ErrUserIDTooLong = errors.New("cometgo: revalidate: user ID too long")

// maxNotifyPayload is the longest pg_notify payload Postgres accepts.
const maxNotifyPayload = 7999

type evtMsg struct {
	T     string          `json:"t"`
	Seq   uint64          `json:"seq"`
	S     uint64          `json:"s"`
	Topic string          `json:"topic"`
	Data  json.RawMessage `json:"data"`
}

func (m *evtMsg) setSeq(s uint64) { m.Seq = s }

// evtNote is an event as it travels through pg_notify: the observer key's
// parts, topic and data. ID makes every payload unique: Postgres delivers
// only one of several identical notifications sent in one transaction.
type evtNote struct {
	ID    string          `json:"i"`
	Pub   string          `json:"p"`
	Args  string          `json:"a"`
	Scope string          `json:"s"`
	Topic string          `json:"t"`
	Data  json.RawMessage `json:"d"`
}

// observerKey identifies the observer for (publication, canonical args,
// scopeKey). engine.bind and event delivery both use it.
func observerKey(pub, argsKey, scope string) string {
	return pub + "\x00" + argsKey + "\x00" + scope
}

// revalChannel is the Postgres channel Revalidate travels on, so it reaches
// sessions on every node listening to the same database. The payload is
// the user ID.
const revalChannel = "comet_reval"

// Revalidate re-runs the publication fn of every live subscription held by
// sessions whose Identity.UserID is userID, including sessions waiting to be
// resumed, on every node listening to this database. A sub whose fn now
// returns an error ends with that error; the others are untouched. Call it
// after changing a permission that fn checks but that its query does not
// read (a query that reads the permission table, listed in Tables, picks up
// the change by itself).
//
// Only an error ends a sub: a fn that narrows its Query instead (say, to the
// teams the user still belongs to) changes nothing, since the observer keeps
// the Query it was created with. Any error ends the sub, a transient one too.
//
// It sends a notification and returns; each node re-checks on its next tick
// after receiving it, against what fn reads then, so call it after the
// change has committed. Inside a method, use Call.RevalidateAfterCommit. A
// node whose listener is reconnecting when the notification is sent misses
// it, as with events. An empty userID does nothing.
func (a *App) Revalidate(ctx context.Context, userID string) error {
	if userID == "" {
		return nil // anonymous sessions have no per-user permission to revoke
	}
	if len(userID) > maxNotifyPayload {
		return ErrUserIDTooLong
	}
	_, err := a.db.Exec(ctx, "SELECT pg_notify($1, $2)", revalChannel, userID)
	return err
}

// RevalidateAfterCommit is Revalidate inside the method's transaction: the
// re-check is sent only if the transaction commits, and runs once the
// method's writes are visible. A sub it ends gets its end before the
// method's res (unless the call was sent with noFence).
func (c *Call) RevalidateAfterCommit(userID string) error {
	if c.app == nil {
		return errors.New("cometgo: RevalidateAfterCommit needs a Call made by the framework")
	}
	if userID == "" {
		return nil
	}
	if len(userID) > maxNotifyPayload {
		return ErrUserIDTooLong // checked here: a failed pg_notify would abort c.Tx
	}
	// txid_current gives the transaction an ID even if the method writes
	// nothing else, so a server that skips the fence for ID-less calls
	// still sends the end before the res. pg_notify alone assigns none.
	_, err := c.Tx.Exec(c.Context, "SELECT pg_notify($1, $2), txid_current()", revalChannel, userID)
	return err
}

// Emit sends an ephemeral event to every subscription of the publication
// pubName with the given args and scope: the subs that share one observer.
// args must encode to the same JSON value the client subscribed with. scope
// is the subscribers' Identity.Scope() (their UserID when they have no
// ScopeKey) for a ScopeUser publication and is ignored for ScopeShared.
// The bare ScopeKey is not a scope: an event sent to it reaches no one.
// The publication must have PubOptions.Events set.
//
// Delivery is at-most-once and best-effort: an event reaches the subs bound
// at the moment the engine processes it, after any changes committed before
// the emit, and is never replayed to a client that resyncs (PROTOCOL.md §7.1).
// To emit about data a method writes, use Call.EmitAfterCommit instead, so
// the event cannot arrive before that data or for a write that rolled back.
func (a *App) Emit(ctx context.Context, pubName string, args any, scope, topic string, data any) error {
	payload, err := a.evtPayload(pubName, args, scope, topic, data)
	if err != nil {
		return err
	}
	_, err = a.db.Exec(ctx, "SELECT pg_notify($1, $2)", evtChannel, payload)
	return err
}

// EmitAfterCommit is Emit inside the method's transaction, with the same
// arguments (scope is the subscribers' Identity.Scope()): the event is sent
// only if the transaction commits, and reaches subscribers after the tx
// carrying the method's writes and before the method's res (unless the call
// was sent with noFence, whose res does not wait).
func (c *Call) EmitAfterCommit(pubName string, args any, scope, topic string, data any) error {
	if c.app == nil {
		return errors.New("cometgo: EmitAfterCommit needs a Call made by the framework")
	}
	payload, err := c.app.evtPayload(pubName, args, scope, topic, data)
	if err != nil {
		return err
	}
	// txid_current assigns the transaction an ID, which keeps the call's
	// fence (a call with no ID skips it): the evt must precede the res.
	// pg_notify alone assigns none before commit.
	_, err = c.Tx.Exec(c.Context, "SELECT pg_notify($1, $2), txid_current()", evtChannel, payload)
	return err
}

func (a *App) evtPayload(pubName string, args any, scope, topic string, data any) (string, error) {
	pub := a.pubs[pubName]
	if pub == nil {
		return "", fmt.Errorf("cometgo: emit: unknown publication %q", pubName)
	}
	if !pub.opts.Events {
		return "", fmt.Errorf("cometgo: emit: publication %q does not have Events enabled", pubName)
	}
	if pub.opts.Scope == ScopeUser && scope == "" {
		// "" is the anonymous scope; a missing scope must not reach it.
		return "", fmt.Errorf("cometgo: emit: publication %q is ScopeUser and needs the subscribers' Identity.Scope() as scope", pubName)
	}
	rawArgs, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("cometgo: emit: args: %w", err)
	}
	argsKey, err := canonicalJSON(rawArgs)
	if err != nil {
		return "", fmt.Errorf("cometgo: emit: %w", err)
	}
	rawData, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("cometgo: emit: data: %w", err)
	}
	n := evtNote{ID: newID(6), Pub: pubName, Args: argsKey, Scope: scopeKey(pub, scope), Topic: topic, Data: rawData}
	b, err := json.Marshal(&n)
	if err != nil {
		return "", err
	}
	if len(b) > maxEvtPayload {
		return "", fmt.Errorf("%w: %d bytes encoded, limit %d", ErrEventTooLarge, len(b), maxEvtPayload)
	}
	return string(b), nil
}

// scopeKey is the scope part of an observer key (see engine.bind).
func scopeKey(pub *publication, scope string) string {
	if pub.opts.Scope == ScopeUser {
		return "u:" + scope
	}
	return ""
}

func parseEvt(payload string) (*evtNote, bool) {
	var n evtNote
	if err := json.Unmarshal([]byte(payload), &n); err != nil || n.Pub == "" {
		return nil, false
	}
	return &n, true
}

// deliverEvents sends each event drained this tick, and each held one, to
// every sub of its observer whose client negotiated the evt cap, once the
// observer reflects every change notified before the event. An observer's
// events go out in order; one still stale holds them. It runs after the
// tick's tx is enqueued and before its fences are released, so an event
// follows the data committed before it and precedes the res of the call
// that emitted it.
func (e *engine) deliverEvents(evts []heldEvt) {
	// Held events are older than this drain's, so they go first.
	for obs := range e.heldEvts {
		if e.observers[obs.key] != obs {
			delete(e.heldEvts, obs) // unsubscribed: at-most-once means dropped
			continue
		}
		for len(obs.evts) > 0 && obs.freshPast(obs.evts[0].n) {
			e.sendEvent(obs, obs.evts[0].note)
			obs.evts = obs.evts[1:]
		}
		if len(obs.evts) == 0 {
			obs.evts = nil
			delete(e.heldEvts, obs)
		}
	}
	for _, h := range evts {
		obs := e.observers[observerKey(h.note.Pub, h.note.Args, h.note.Scope)]
		if obs == nil || !obs.pub.opts.Events {
			continue // nobody subscribed: at-most-once means dropped
		}
		if len(obs.evts) == 0 && obs.freshPast(h.n) {
			e.sendEvent(obs, h.note)
			continue
		}
		obs.evts = append(obs.evts, h)
		e.heldEvts[obs] = true
	}
}

func (e *engine) sendEvent(obs *observer, n *evtNote) {
	// Map order: the order across subs means nothing to a client, and
	// each sub still gets the events in emit order.
	for sub := range obs.subs {
		// A coalescing connection (§8.1) gets no events: a held sub is
		// behind the data the event follows, and queueing them would grow
		// its memory with the event rate. Nor does a sub whose snapshot is
		// unsent, or one of a connection whose identity change is carried
		// (tick). At-most-once allows all three.
		if sub.c.evt && !e.coalescing[sub.c] && !e.rebinding[sub.c] && !sub.needInit && !sub.needsReset {
			sub.c.sendEvent(&evtMsg{T: "evt", S: sub.s, Topic: n.Topic, Data: n.Data})
		}
	}
}

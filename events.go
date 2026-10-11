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
// scope key). engine.bind and event delivery both use it.
func observerKey(pub, argsKey, scope string) string {
	return pub + "\x00" + argsKey + "\x00" + scope
}

// Emit sends an ephemeral event to every subscription of the publication
// pubName with the given args and scope: the subs that share one observer.
// args must encode to the same JSON value the client subscribed with. scope
// is the user ID for a ScopeUser publication and is ignored for ScopeShared.
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

// EmitAfterCommit is Emit inside the method's transaction: the event is sent
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
	_, err = c.Tx.Exec(c.Context, "SELECT pg_notify($1, $2)", evtChannel, payload)
	if err == nil {
		c.emitted = true
	}
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
		// "" is the anonymous scope; a missing user ID must not reach it.
		return "", fmt.Errorf("cometgo: emit: publication %q is ScopeUser and needs a user ID as scope", pubName)
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
func scopeKey(pub *publication, userID string) string {
	if pub.opts.Scope == ScopeUser {
		return "u:" + userID
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

// deliverEvents sends each event to every sub of its observer whose client
// negotiated the evt cap, except on coalescing connections. It runs after the tick's tx is enqueued and before
// its fences are released, so an event follows the data committed before it
// and precedes the res of the call that emitted it.
func (e *engine) deliverEvents(evts []*evtNote) {
	for _, n := range evts {
		obs := e.observers[observerKey(n.Pub, n.Args, n.Scope)]
		if obs == nil || !obs.pub.opts.Events {
			continue // nobody subscribed: at-most-once means dropped
		}
		for _, sub := range obs.sortedSubs() {
			// A coalescing connection (§8.1) gets no events: a held sub is
			// behind the data the event follows, and queueing them would
			// grow its memory with the event rate. At-most-once allows it.
			if sub.c.evt && !e.coalescing[sub.c] {
				sub.c.sendEvent(&evtMsg{T: "evt", S: sub.s, Topic: n.Topic, Data: n.Data})
			}
		}
	}
}

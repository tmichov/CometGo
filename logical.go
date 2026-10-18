package cometgo

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
)

// This file turns pgoutput (protocol 1, messages 'true') into the engine's
// notifications for the logical feed (PROTOCOL.md §11.1,
// docs/design/m4-logical-feed.md §2–§4): decoder parses each transaction,
// gate hands it to the engine once it is visible.

// maxTxKeys is the trigger feed's limit: more changed rows of one (op,
// table) in a transaction make the table dirty.
const maxTxKeys = 1000

// decodedTx is one transaction's notifications, in order. xid is 0 for a
// non-transactional message, which needs no visibility wait.
type decodedTx struct {
	xid   uint32
	full  uint64        // xid, epoch-extended by the gate; 0 for a message
	end   pglogrepl.LSN // the transaction's end LSN; 0 for a message
	notes []notification
}

type decRel struct {
	name string
	// key holds, in primary-key order, each key column's index in the
	// relation's tuples; nil if the table has no primary key or a key column
	// is not published, so its changes make it dirty.
	key   []int
	types []uint32
}

// decoder turns pgoutput messages into decodedTx values. Row changes are
// grouped per (op, table) and flushed before each transactional message
// and at commit, so a message follows the rows written before it, as with
// NOTIFY.
type decoder struct {
	// pkColumns returns a relation's primary-key column names in key order
	// (pkColumns); it runs when a Relation message arrives.
	pkColumns func(ctx context.Context, relID uint32) ([]string, error)

	rels   map[uint32]*decRel
	xid    uint32
	groups []*keyGroup // this transaction's, in first-seen order
	notes  []notification

	// Protocol 2 streaming (design §8): a large transaction arrives in
	// chunks while it runs. inStream is set between Stream Start and Stream
	// Stop; streams holds each streamed transaction, by top-level xid,
	// until its Stream Commit or Abort.
	inStream bool
	streams  map[uint32]*streamTx
	streamID uint32 // the transaction of the current chunk
}

// streamTx is a streamed transaction folded down: the tables it changed,
// each reported dirty at its commit (its keys would cost what streaming
// saves), and its comet messages.
type streamTx struct {
	all    bool // a change to a relation never described
	tables []string
	msgs   []notification
}

type keyGroup struct {
	op, table string
	dirty     bool
	seen      map[string]bool
	keys      []string // JSON arrays, deduplicated as the triggers' DISTINCT
}

func newDecoder(pkColumns func(context.Context, uint32) ([]string, error)) *decoder {
	return &decoder{pkColumns: pkColumns, rels: map[uint32]*decRel{}, streams: map[uint32]*streamTx{}}
}

// handle applies one message. It returns a finished transaction at COMMIT,
// and a non-transactional comet message at once.
func (d *decoder) handle(ctx context.Context, m pglogrepl.Message) (*decodedTx, error) {
	// Protocol 2 wraps each message; outside a stream it is the same.
	switch v := m.(type) {
	case *pglogrepl.StreamStartMessageV2:
		d.inStream, d.streamID = true, v.Xid
		if d.streams[v.Xid] == nil {
			d.streams[v.Xid] = &streamTx{}
		}
		return nil, nil
	case *pglogrepl.StreamStopMessageV2:
		d.inStream = false
		return nil, nil
	case *pglogrepl.StreamAbortMessageV2:
		st := d.streams[v.Xid]
		if v.SubXid == v.Xid || st == nil {
			delete(d.streams, v.Xid)
			return nil, nil
		}
		// A savepoint rolled back. Its tables stay dirty (a few more
		// re-runs, never a missed change). Its messages cannot be told
		// apart: pgoutput streams them under the top-level xid. So every
		// event streamed so far goes, since one from the savepoint must
		// not be delivered and events are at-most-once; re-checks stay,
		// since an extra one only re-runs fn; and the fence, always the
		// last statement, comes after any savepoint.
		st.msgs = slices.DeleteFunc(st.msgs, func(n notification) bool { return n.evt != nil })
		return nil, nil
	case *pglogrepl.StreamCommitMessageV2:
		st := d.streams[v.Xid]
		delete(d.streams, v.Xid)
		tx := &decodedTx{xid: v.Xid, end: v.TransactionEndLSN}
		if st != nil {
			if st.all {
				tx.notes = append(tx.notes, notification{all: true})
			}
			for _, t := range st.tables {
				tx.notes = append(tx.notes, notification{table: t})
			}
			tx.notes = append(tx.notes, st.msgs...)
		}
		return tx, nil
	case *pglogrepl.RelationMessageV2:
		m = &v.RelationMessage
	case *pglogrepl.InsertMessageV2:
		if d.inStream {
			d.streamTable(v.RelationID)
			return nil, nil
		}
		m = &v.InsertMessage
	case *pglogrepl.UpdateMessageV2:
		if d.inStream {
			d.streamTable(v.RelationID)
			return nil, nil
		}
		m = &v.UpdateMessage
	case *pglogrepl.DeleteMessageV2:
		if d.inStream {
			d.streamTable(v.RelationID)
			return nil, nil
		}
		m = &v.DeleteMessage
	case *pglogrepl.TruncateMessageV2:
		if d.inStream {
			for _, id := range v.RelationIDs {
				d.streamTable(id)
			}
			return nil, nil
		}
		m = &v.TruncateMessage
	case *pglogrepl.LogicalDecodingMessageV2:
		if d.inStream && v.Transactional {
			if v.Prefix == "comet" {
				if n, ok := parseMessage(string(v.Content)); ok {
					st := d.streams[d.streamID]
					st.msgs = append(st.msgs, n)
				}
			}
			return nil, nil
		}
		m = &v.LogicalDecodingMessage
	case *pglogrepl.TypeMessageV2:
		return nil, nil
	}
	switch m := m.(type) {
	case *pglogrepl.RelationMessage:
		pk, err := d.pkColumns(ctx, m.RelationID)
		if err != nil {
			return nil, err
		}
		r := &decRel{name: m.Namespace + "." + m.RelationName}
		for _, c := range m.Columns {
			r.types = append(r.types, c.DataType)
		}
		for _, name := range pk {
			i := slices.IndexFunc(m.Columns, func(c *pglogrepl.RelationMessageColumn) bool { return c.Name == name })
			if i < 0 {
				r.key = nil
				break
			}
			r.key = append(r.key, i)
		}
		d.rels[m.RelationID] = r
	case *pglogrepl.BeginMessage:
		d.xid, d.groups, d.notes = m.Xid, nil, nil
	case *pglogrepl.InsertMessage:
		d.add("I", m.RelationID, m.Tuple)
	case *pglogrepl.UpdateMessage:
		// OldTuple is there when the key changed, or always under REPLICA
		// IDENTITY FULL: the old and new keys are reported, as the triggers
		// report the union of old and new rows.
		d.add("U", m.RelationID, m.OldTuple, m.NewTuple)
	case *pglogrepl.DeleteMessage:
		d.add("D", m.RelationID, m.OldTuple)
	case *pglogrepl.TruncateMessage:
		d.flush()
		for _, id := range m.RelationIDs {
			if r := d.rels[id]; r != nil {
				d.notes = append(d.notes, notification{table: r.name})
			} else {
				d.notes = append(d.notes, notification{all: true})
			}
		}
	case *pglogrepl.LogicalDecodingMessage:
		if m.Prefix != "comet" {
			break
		}
		n, ok := parseMessage(string(m.Content))
		if !m.Transactional {
			// Emit and Revalidate in their own statement: no transaction.
			if !ok {
				return nil, nil
			}
			return &decodedTx{notes: []notification{n}}, nil
		}
		d.flush()
		if ok {
			d.notes = append(d.notes, n)
		}
	case *pglogrepl.CommitMessage:
		d.flush()
		tx := &decodedTx{xid: d.xid, end: m.TransactionEndLSN, notes: d.notes}
		d.xid, d.groups, d.notes = 0, nil, nil
		return tx, nil
	}
	return nil, nil
}

// streamTable records a change to relation relID in the current chunk's
// transaction.
func (d *decoder) streamTable(relID uint32) {
	st := d.streams[d.streamID]
	r := d.rels[relID]
	switch {
	case r == nil:
		st.all = true
	case !slices.Contains(st.tables, r.name):
		st.tables = append(st.tables, r.name)
	}
}

// parseMessage parses a comet message: f|token, e|event JSON or v|userID.
// ok is false for one to drop, as the notify feed drops a bad event or an
// empty re-check; anything unrecognised marks everything dirty.
func parseMessage(content string) (n notification, ok bool) {
	kind, rest, _ := strings.Cut(content, "|")
	switch kind {
	case "f":
		return notification{token: rest}, true
	case "e":
		evt, ok := parseEvt(rest)
		return notification{evt: evt}, ok
	case "v":
		return notification{reval: rest}, rest != ""
	}
	return notification{all: true}, true
}

func (d *decoder) add(op string, relID uint32, tuples ...*pglogrepl.TupleData) {
	r := d.rels[relID]
	if r == nil {
		// No Relation message before its change: pgoutput always sends
		// one, so this is a protocol surprise; dirty is never wrong.
		d.flush()
		d.notes = append(d.notes, notification{all: true})
		return
	}
	var g *keyGroup
	for _, x := range d.groups {
		if x.op == op && x.table == r.name {
			g = x
		}
	}
	if g == nil {
		g = &keyGroup{op: op, table: r.name, seen: map[string]bool{}}
		d.groups = append(d.groups, g)
	}
	if g.dirty {
		return
	}
	for _, t := range tuples {
		if t == nil {
			continue
		}
		k, ok := encodeKey(r, t)
		if !ok {
			g.dirty = true
			return
		}
		if !g.seen[k] {
			g.seen[k] = true
			g.keys = append(g.keys, k)
		}
	}
	if len(g.keys) > maxTxKeys {
		g.dirty, g.keys, g.seen = true, nil, nil
	}
}

func (d *decoder) flush() {
	for _, g := range d.groups {
		n := notification{table: g.table}
		if !g.dirty {
			n.keys = "[" + strings.Join(g.keys, ",") + "]"
		}
		d.notes = append(d.notes, n)
	}
	d.groups = nil
}

// encodeKey encodes a tuple's primary key as the triggers' jsonb_build_array
// does (§10): int2 and int4 as numbers, int8 and every other type as the
// string of its text form. ok is false when the key cannot be read from the
// tuple (no primary key, a null or unchanged-TOAST key column, or binary
// data), and the table is then reported dirty instead.
func encodeKey(r *decRel, t *pglogrepl.TupleData) (string, bool) {
	if len(r.key) == 0 {
		return "", false
	}
	var sb strings.Builder
	sb.WriteByte('[')
	for j, i := range r.key {
		if i >= len(t.Columns) || t.Columns[i].DataType != pglogrepl.TupleDataTypeText {
			return "", false
		}
		if j > 0 {
			sb.WriteByte(',')
		}
		v := string(t.Columns[i].Data)
		switch r.types[i] {
		case 21, 23: // int2, int4
			sb.WriteString(v)
		default:
			b, _ := json.Marshal(v)
			sb.Write(b)
		}
	}
	sb.WriteByte(']')
	return sb.String(), true
}

// pkColumns returns a table's primary-key column names in key order, as
// the triggers build their key arrays; none if it has no primary key.
func pkColumns(ctx context.Context, q querier, relID uint32) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT a.attname
		FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		WHERE i.indrelid = $1 AND i.indisprimary
		ORDER BY array_position(i.indkey::int2[], a.attnum)`, relID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// pgSnapshot is pg_current_snapshot(): every xid below xmin has ended,
// every xid from xmax on has not, and xip lists the ones in between still
// running. A transaction that has committed but waits for a synchronous
// standby is still in xip.
type pgSnapshot struct {
	xmin, xmax uint64
	xip        []uint64
}

func currentSnapshot(ctx context.Context, q querier) (pgSnapshot, error) {
	rows, err := q.Query(ctx, `
		SELECT pg_snapshot_xmin(s)::text, pg_snapshot_xmax(s)::text, ARRAY(SELECT pg_snapshot_xip(s)::text)
		FROM pg_current_snapshot() s`)
	if err != nil {
		return pgSnapshot{}, err
	}
	var s pgSnapshot
	var xmin, xmax string
	var xip []string
	if _, err := pgx.ForEachRow(rows, []any{&xmin, &xmax, &xip}, func() error { return nil }); err != nil {
		return pgSnapshot{}, err
	}
	if s.xmin, err = strconv.ParseUint(xmin, 10, 64); err != nil {
		return pgSnapshot{}, err
	}
	if s.xmax, err = strconv.ParseUint(xmax, 10, 64); err != nil {
		return pgSnapshot{}, err
	}
	for _, x := range xip {
		v, err := strconv.ParseUint(x, 10, 64)
		if err != nil {
			return pgSnapshot{}, err
		}
		s.xip = append(s.xip, v)
	}
	return s, nil
}

// extend gives a 32-bit xid from pgoutput its epoch, from the snapshot's
// xmax: a decoded transaction has committed, so its xid is below xmax and
// within 2^31 of it. Never compare a raw 32-bit xid with an xid8. An xid
// equal to xmax's low bits stays at xmax, which is never visible.
func (s pgSnapshot) extend(xid uint32) uint64 {
	full := s.xmax&^0xffffffff | uint64(xid)
	if full > s.xmax && full >= 1<<32 {
		full -= 1 << 32
	}
	return full
}

func (s pgSnapshot) visible(full uint64) bool {
	return full < s.xmin || full < s.xmax && !slices.Contains(s.xip, full)
}

// gate hands decoded transactions to deliver in commit order, each once
// its xid is visible in a fresh snapshot (design §4): a transaction is
// decoded once its commit record is flushed, but other sessions, the
// engine's tick among them, see it only later (after a synchronous-standby
// wait, for one). A held transaction holds every later one. push never
// blocks, so the replication connection keeps being read and answered
// while the gate holds.
type gate struct {
	// snapshot runs on the feed's own connection: a pool saturated by
	// method calls must not stall the feed.
	snapshot func(context.Context) (pgSnapshot, error)
	// deliver gets each transaction with its notifications' xid set to the
	// epoch-extended xid.
	deliver func(decodedTx)
	poll    time.Duration

	mu    sync.Mutex
	queue []decodedTx
	wake  chan struct{}
}

func newGate(snapshot func(context.Context) (pgSnapshot, error), deliver func(decodedTx)) *gate {
	return &gate{snapshot: snapshot, deliver: deliver, poll: 2 * time.Millisecond, wake: make(chan struct{}, 1)}
}

func (g *gate) push(tx decodedTx) {
	g.mu.Lock()
	g.queue = append(g.queue, tx)
	g.mu.Unlock()
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// run delivers until ctx ends, or a snapshot fails; the queue survives for
// the next run.
func (g *gate) run(ctx context.Context) error {
	for {
		g.mu.Lock()
		queue := slices.Clone(g.queue)
		g.mu.Unlock()
		if len(queue) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-g.wake:
				continue
			}
		}
		var snap *pgSnapshot
		var err error
		n := 0
		for _, tx := range queue {
			var full uint64
			if tx.xid != 0 {
				if snap == nil {
					var s pgSnapshot
					if s, err = g.snapshot(ctx); err != nil {
						break
					}
					snap = &s
				}
				full = snap.extend(tx.xid)
				if !snap.visible(full) {
					break
				}
			}
			tx.full = full
			for i := range tx.notes {
				tx.notes[i].xid = full
			}
			g.deliver(tx)
			n++
		}
		// Trimmed before an error returns too: what went out must not go
		// out again on the next run.
		g.mu.Lock()
		g.queue = g.queue[n:]
		g.mu.Unlock()
		if err != nil {
			return err
		}
		if n < len(queue) {
			// Held: poll again soon, without backoff; visibility follows the
			// commit by a standby round trip at most, in the normal case.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(g.poll):
			}
		}
	}
}

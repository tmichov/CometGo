// Command feedspike is the M4 discovery spike (CMTG-31): a minimal pgoutput
// consumer that turns logical decoding into the engine's existing §11
// notification payloads, run against a fixed list of SQL scenarios. It is
// not part of the server and is not meant to be merged.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

const maxKeys = 1000 // as the triggers: more changed rows make the table dirty

type rel struct {
	name  string   // schema.table
	types []uint32 // column type OIDs; the publication lists key columns only
}

// decoder turns one transaction's pgoutput messages into §11 payloads. Row
// changes are grouped per (op, table) and flushed before a transactional
// message (fence, event, re-check) and at commit, so a message is always
// delivered after the rows written before it, as with NOTIFY.
type decoder struct {
	rels  map[uint32]rel
	order []string // "op|table" in first-seen order
	keys  map[string][]json.RawMessage
	dirty map[string]bool
	out   []string // payloads of the transaction, sent at commit
}

func newDecoder() *decoder {
	return &decoder{rels: map[uint32]rel{}, keys: map[string][]json.RawMessage{}, dirty: map[string]bool{}}
}

func (d *decoder) key(r rel, t *pglogrepl.TupleData) json.RawMessage {
	parts := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		v := string(c.Data)
		switch r.types[i] {
		case 21, 23: // int2, int4: JSON numbers
			parts[i] = v
		default: // int8 travels as a string (§10), as do text and uuid
			b, _ := json.Marshal(v)
			parts[i] = string(b)
		}
	}
	return json.RawMessage("[" + strings.Join(parts, ",") + "]")
}

func (d *decoder) add(op string, relID uint32, tuples ...*pglogrepl.TupleData) {
	r := d.rels[relID]
	g := op + "|" + r.name
	if _, seen := d.keys[g]; !seen && !d.dirty[g] {
		d.order = append(d.order, g)
		d.keys[g] = nil
	}
	if d.dirty[g] {
		return
	}
	for _, t := range tuples {
		if t != nil {
			d.keys[g] = append(d.keys[g], d.key(r, t))
		}
	}
	if len(d.keys[g]) > maxKeys {
		d.dirty[g], d.keys[g] = true, nil
	}
}

func (d *decoder) flush() {
	for _, g := range d.order {
		op, table, _ := strings.Cut(g, "|")
		if d.dirty[g] {
			d.out = append(d.out, "1|t|"+op+"|"+table)
			continue
		}
		b, _ := json.Marshal(d.keys[g])
		// shortcut: no dedupe of keys or 7,500-byte split; the trigger feed
		// dedupes with DISTINCT, the engine tolerates duplicates.
		d.out = append(d.out, "1|r|"+op+"|"+table+"|"+string(b))
	}
	d.order, d.keys, d.dirty = nil, map[string][]json.RawMessage{}, map[string]bool{}
}

// handle applies one message; at commit it returns the transaction's
// payloads with the commit LSN.
func (d *decoder) handle(m pglogrepl.Message) (done []string, commit pglogrepl.LSN) {
	switch m := m.(type) {
	case *pglogrepl.RelationMessage:
		r := rel{name: m.Namespace + "." + m.RelationName}
		for _, c := range m.Columns {
			r.types = append(r.types, c.DataType)
		}
		d.rels[m.RelationID] = r
	case *pglogrepl.InsertMessage:
		d.add("I", m.RelationID, m.Tuple)
	case *pglogrepl.UpdateMessage:
		// OldTuple is the old key, present only when the key changed: the
		// union of old and new keys, as the trigger feed reports.
		d.add("U", m.RelationID, m.OldTuple, m.NewTuple)
	case *pglogrepl.DeleteMessage:
		d.add("D", m.RelationID, m.OldTuple)
	case *pglogrepl.TruncateMessage:
		d.flush()
		for _, id := range m.RelationIDs {
			d.out = append(d.out, "1|t|T|"+d.rels[id].name)
		}
	case *pglogrepl.LogicalDecodingMessage:
		if m.Prefix != "comet" {
			break
		}
		if !m.Transactional {
			// Sent at once, outside any transaction: Emit and Revalidate.
			return []string{"msg(now) " + string(m.Content)}, 0
		}
		d.flush()
		d.out = append(d.out, "msg "+string(m.Content))
	case *pglogrepl.CommitMessage:
		d.flush()
		done, d.out = d.out, nil
		return done, m.CommitLSN
	}
	return nil, 0
}

type feed struct {
	mu   sync.Mutex
	txns [][]string
	got  chan struct{}
}

func (f *feed) take(timeout time.Duration) [][]string {
	deadline := time.After(timeout)
	for {
		f.mu.Lock()
		if len(f.txns) > 0 {
			t := f.txns
			f.txns = nil
			f.mu.Unlock()
			// Let the rest of a burst arrive.
			time.Sleep(200 * time.Millisecond)
			f.mu.Lock()
			t = append(t, f.txns...)
			f.txns = nil
			f.mu.Unlock()
			return t
		}
		f.mu.Unlock()
		select {
		case <-f.got:
		case <-deadline:
			return nil
		}
	}
}

func stream(ctx context.Context, conn *pgconn.PgConn, slot string, start pglogrepl.LSN, f *feed) error {
	err := pglogrepl.StartReplication(ctx, conn, slot, start, pglogrepl.StartReplicationOptions{
		PluginArgs: []string{"proto_version '1'", "publication_names 'feedspike'", "messages 'true'"},
	})
	if err != nil {
		return err
	}
	d := newDecoder()
	confirmed := start
	nextStatus := time.Now()
	for ctx.Err() == nil {
		if time.Now().After(nextStatus) {
			if err := pglogrepl.SendStandbyStatusUpdate(ctx, conn, pglogrepl.StandbyStatusUpdate{WALWritePosition: confirmed}); err != nil {
				return err
			}
			nextStatus = time.Now().Add(5 * time.Second)
		}
		rctx, cancel := context.WithDeadline(ctx, nextStatus)
		raw, err := conn.ReceiveMessage(rctx)
		cancel()
		if pgconn.Timeout(err) {
			continue
		}
		if err != nil {
			return err
		}
		cd, ok := raw.(*pgproto3.CopyData)
		if !ok {
			continue
		}
		switch cd.Data[0] {
		case pglogrepl.PrimaryKeepaliveMessageByteID:
			pk, err := pglogrepl.ParsePrimaryKeepaliveMessage(cd.Data[1:])
			if err == nil && pk.ReplyRequested {
				nextStatus = time.Time{}
			}
		case pglogrepl.XLogDataByteID:
			x, err := pglogrepl.ParseXLogData(cd.Data[1:])
			if err != nil {
				return err
			}
			m, err := pglogrepl.Parse(x.WALData)
			if err != nil {
				return err
			}
			if out, commit := d.handle(m); out != nil {
				if commit != 0 {
					confirmed = commit
				}
				f.mu.Lock()
				f.txns = append(f.txns, out)
				f.mu.Unlock()
				select {
				case f.got <- struct{}{}:
				default:
				}
			}
		}
	}
	return nil
}

func replConn(ctx context.Context, dsn string) (*pgconn.PgConn, error) {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return pgconn.Connect(ctx, dsn+sep+"replication=database")
}

func main() {
	ctx := context.Background()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://comet:comet@localhost:54329/comet"
	}
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close(ctx)
	for _, s := range []string{
		"DROP PUBLICATION IF EXISTS feedspike",
		"DROP SCHEMA IF EXISTS spike CASCADE",
		"CREATE SCHEMA spike",
		"CREATE TABLE spike.items (id bigint PRIMARY KEY, n int NOT NULL DEFAULT 0, note text)",
		"CREATE TABLE spike.comp (org text, id int, v int, PRIMARY KEY (org, id))",
		// Key columns only: the feed needs no more, and it keeps the stream small.
		"CREATE PUBLICATION feedspike FOR TABLE spike.items (id), spike.comp (org, id)",
	} {
		if _, err := db.Exec(ctx, s); err != nil {
			log.Fatalf("%s: %v", s, err)
		}
	}

	rc, err := replConn(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	t0 := time.Now()
	slot, err := pglogrepl.CreateReplicationSlot(ctx, rc, "feedspike", "pgoutput",
		pglogrepl.CreateReplicationSlotOptions{Temporary: true, Mode: pglogrepl.LogicalReplication, SnapshotAction: "NOEXPORT_SNAPSHOT"})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("temporary slot created in %s, consistent point %s\n\n", time.Since(t0).Round(time.Millisecond), slot.ConsistentPoint)
	start, _ := pglogrepl.ParseLSN(slot.ConsistentPoint)
	f := &feed{got: make(chan struct{}, 1)}
	sctx, stop := context.WithCancel(ctx)
	go func() {
		if err := stream(sctx, rc, "feedspike", start, f); err != nil && sctx.Err() == nil {
			log.Fatal("stream: ", err)
		}
	}()

	type step struct {
		name string
		sql  []string // one transaction; "ROLLBACK" as last ends it so
	}
	steps := []step{
		{"insert 3 rows", []string{"INSERT INTO spike.items (id) VALUES (1), (2), (3)"}},
		{"update, key unchanged", []string{"UPDATE spike.items SET n = n + 1 WHERE id = 1"}},
		{"update, key changed", []string{"UPDATE spike.items SET id = 100 WHERE id = 2"}},
		{"delete", []string{"DELETE FROM spike.items WHERE id = 3"}},
		{"fenced call: write, then a transactional message", []string{
			"UPDATE spike.items SET n = n + 1 WHERE id = 1",
			"SELECT pg_logical_emit_message(true, 'comet', 'f|node1:7')",
		}},
		{"two statements, message between", []string{
			"INSERT INTO spike.items (id) VALUES (10)",
			"SELECT pg_logical_emit_message(true, 'comet', 'e|after 10')",
			"INSERT INTO spike.items (id) VALUES (11)",
		}},
		{"non-transactional message in a transaction that rolls back", []string{
			"INSERT INTO spike.items (id) VALUES (20)",
			"SELECT pg_logical_emit_message(false, 'comet', 'e|emit now')",
			"ROLLBACK",
		}},
		{"2000 rows in one transaction", []string{"INSERT INTO spike.items (id) SELECT g FROM generate_series(1000, 2999) g"}},
		{"composite key", []string{"INSERT INTO spike.comp VALUES ('acme', 1, 0), ('globex', 1, 0)"}},
		{"truncate", []string{"TRUNCATE spike.comp"}},
	}
	for _, s := range steps {
		fmt.Printf("## %s\n", s.name)
		tx, err := db.Begin(ctx)
		if err != nil {
			log.Fatal(err)
		}
		rollback := false
		for _, q := range s.sql {
			if q == "ROLLBACK" {
				rollback = true
				continue
			}
			if _, err := tx.Exec(ctx, q); err != nil {
				log.Fatalf("%s: %v", q, err)
			}
		}
		if rollback {
			err = tx.Rollback(ctx)
		} else {
			err = tx.Commit(ctx)
		}
		if err != nil {
			log.Fatal(err)
		}
		got := f.take(3 * time.Second)
		if len(got) == 0 {
			fmt.Println("  (nothing)")
		}
		for i, txn := range got {
			for _, p := range txn {
				if len(p) > 120 {
					p = p[:120] + "…"
				}
				fmt.Printf("  tx%d: %s\n", i+1, p)
			}
		}
		fmt.Println()
	}

	// A slot becomes consistent only once every transaction that was
	// running when it started has finished: a long transaction holds up a
	// node's feed at start and after every reconnect.
	fmt.Println("## creating a slot while a transaction holding an xid is open for 2 s")
	long, err := db.Begin(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := long.Exec(ctx, "SELECT txid_current()"); err != nil {
		log.Fatal(err)
	}
	go func() {
		time.Sleep(2 * time.Second)
		long.Commit(ctx)
	}()
	rc2, err := replConn(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	t0 = time.Now()
	if _, err := pglogrepl.CreateReplicationSlot(ctx, rc2, "feedspike2", "pgoutput",
		pglogrepl.CreateReplicationSlotOptions{Temporary: true, Mode: pglogrepl.LogicalReplication, SnapshotAction: "NOEXPORT_SNAPSHOT"}); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("  second slot created in %s\n", time.Since(t0).Round(time.Millisecond))
	rc2.Close(ctx)
	stop()
	rc.Close(ctx)
}

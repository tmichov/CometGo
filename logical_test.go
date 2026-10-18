package cometgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
)

func textTuple(vals ...string) *pglogrepl.TupleData {
	t := &pglogrepl.TupleData{}
	for _, v := range vals {
		c := &pglogrepl.TupleDataColumn{DataType: pglogrepl.TupleDataTypeText, Data: []byte(v)}
		if v == "<null>" {
			c = &pglogrepl.TupleDataColumn{DataType: pglogrepl.TupleDataTypeNull}
		}
		t.Columns = append(t.Columns, c)
	}
	return t
}

// testDecoder knows relation 1, items (id int8 PK, n int4), and relation
// 2, comp (org text, id int4, v int4) whose key is declared (id, org).
func testDecoder(t *testing.T) *decoder {
	t.Helper()
	d := newDecoder(func(_ context.Context, rel uint32) ([]string, error) {
		return map[uint32][]string{1: {"id"}, 2: {"id", "org"}}[rel], nil
	})
	for _, m := range []*pglogrepl.RelationMessage{
		{RelationID: 1, Namespace: "public", RelationName: "items", Columns: []*pglogrepl.RelationMessageColumn{
			{Name: "id", DataType: 20, Flags: 1}, {Name: "n", DataType: 23}}},
		{RelationID: 2, Namespace: "s", RelationName: "comp", Columns: []*pglogrepl.RelationMessageColumn{
			{Name: "org", DataType: 25, Flags: 1}, {Name: "id", DataType: 23, Flags: 1}, {Name: "v", DataType: 23}}},
		{RelationID: 3, Namespace: "public", RelationName: "nopk", Columns: []*pglogrepl.RelationMessageColumn{
			{Name: "a", DataType: 23}}},
	} {
		if _, err := d.handle(context.Background(), m); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// run feeds one transaction (xid 42) and returns its notifications as
// readable strings.
func runTx(t *testing.T, d *decoder, msgs ...pglogrepl.Message) []string {
	t.Helper()
	ctx := context.Background()
	if _, err := d.handle(ctx, &pglogrepl.BeginMessage{Xid: 42}); err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if tx, err := d.handle(ctx, m); err != nil || tx != nil {
			t.Fatalf("mid-transaction: %v, %v", tx, err)
		}
	}
	tx, err := d.handle(ctx, &pglogrepl.CommitMessage{TransactionEndLSN: 99})
	if err != nil || tx == nil || tx.xid != 42 || tx.end != 99 {
		t.Fatalf("commit: %+v, %v", tx, err)
	}
	return noteStrings(tx.notes)
}

func noteStrings(notes []notification) []string {
	var out []string
	for _, n := range notes {
		switch {
		case n.all:
			out = append(out, "all")
		case n.token != "":
			out = append(out, "fence "+n.token)
		case n.evt != nil:
			out = append(out, "evt "+n.evt.Pub)
		case n.reval != "":
			out = append(out, "reval "+n.reval)
		case n.keys != "":
			out = append(out, n.table+" "+n.keys)
		default:
			out = append(out, n.table+" dirty")
		}
	}
	return out
}

func TestDecoderRows(t *testing.T) {
	d := testDecoder(t)
	got := runTx(t, d,
		&pglogrepl.InsertMessage{RelationID: 1, Tuple: textTuple("1", "0")},
		&pglogrepl.InsertMessage{RelationID: 1, Tuple: textTuple("2", "0")},
		// Key unchanged: no old tuple.
		&pglogrepl.UpdateMessage{RelationID: 1, NewTuple: textTuple("1", "5")},
		// Key changed: old key, then the new row.
		&pglogrepl.UpdateMessage{RelationID: 1, OldTupleType: 'K', OldTuple: textTuple("2", "<null>"), NewTuple: textTuple("100", "0")},
		&pglogrepl.DeleteMessage{RelationID: 1, OldTupleType: 'K', OldTuple: textTuple("1", "<null>")},
		// Key in declared order (id, org), not column order; int4 a number.
		&pglogrepl.InsertMessage{RelationID: 2, Tuple: textTuple(`a"cme`, "7", "0")},
		&pglogrepl.InsertMessage{RelationID: 1, Tuple: textTuple("1", "0")}, // same key again: deduplicated
	)
	want := []string{
		`public.items [["1"],["2"]]`,
		`public.items [["1"],["2"],["100"]]`,
		`public.items [["1"]]`,
		`s.comp [[7,"a\"cme"]]`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// Rows are flushed before each transactional message, so a fence or event
// follows the rows written before it.
func TestDecoderMessagesFlush(t *testing.T) {
	d := testDecoder(t)
	got := runTx(t, d,
		&pglogrepl.InsertMessage{RelationID: 1, Tuple: textTuple("1", "0")},
		&pglogrepl.LogicalDecodingMessage{Transactional: true, Prefix: "comet", Content: []byte(`e|{"p":"items","t":"x"}`)},
		&pglogrepl.InsertMessage{RelationID: 1, Tuple: textTuple("2", "0")},
		&pglogrepl.LogicalDecodingMessage{Transactional: true, Prefix: "comet", Content: []byte("v|alice")},
		&pglogrepl.LogicalDecodingMessage{Transactional: true, Prefix: "other", Content: []byte("f|ignored")},
		&pglogrepl.LogicalDecodingMessage{Transactional: true, Prefix: "comet", Content: []byte("e|not json")},
		&pglogrepl.LogicalDecodingMessage{Transactional: true, Prefix: "comet", Content: []byte("v|")},
		&pglogrepl.LogicalDecodingMessage{Transactional: true, Prefix: "comet", Content: []byte("f|node:1|x")},
	)
	want := []string{`public.items [["1"]]`, "evt items", `public.items [["2"]]`, "reval alice", "fence node:1|x"}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}

	// Not transactional (Emit, Revalidate): delivered at once, ungated.
	tx, err := d.handle(context.Background(), &pglogrepl.LogicalDecodingMessage{Prefix: "comet", Content: []byte("v|bob")})
	if err != nil || tx == nil || tx.xid != 0 || !slices.Equal(noteStrings(tx.notes), []string{"reval bob"}) {
		t.Errorf("non-transactional: %+v, %v", tx, err)
	}
	if tx, _ := d.handle(context.Background(), &pglogrepl.LogicalDecodingMessage{Prefix: "comet", Content: []byte("zzz")}); tx == nil || !tx.notes[0].all {
		t.Errorf("unknown message: %+v, want all dirty", tx)
	}
}

func TestDecoderDirty(t *testing.T) {
	d := testDecoder(t)
	var many []pglogrepl.Message
	for i := range maxTxKeys + 1 {
		many = append(many, &pglogrepl.InsertMessage{RelationID: 1, Tuple: textTuple(fmt.Sprint(i), "0")})
	}
	many = append(many,
		&pglogrepl.DeleteMessage{RelationID: 1, OldTuple: textTuple("1", "0")},
		&pglogrepl.InsertMessage{RelationID: 3, Tuple: textTuple("1")},                      // no primary key
		&pglogrepl.UpdateMessage{RelationID: 2, NewTuple: textTuple("acme", "<null>", "0")}, // a null key column
		&pglogrepl.TruncateMessage{RelationIDs: []uint32{2, 1}},
		&pglogrepl.InsertMessage{RelationID: 9, Tuple: textTuple("1")}, // never described
	)
	got := runTx(t, d, many...)
	want := []string{
		"public.items dirty", // 1,001 inserts
		`public.items [["1"]]`,
		"public.nopk dirty",
		"s.comp dirty",
		"s.comp dirty", "public.items dirty", // truncate
		"all",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestSnapshotExtendVisible(t *testing.T) {
	const epoch = uint64(5) << 32
	s := pgSnapshot{xmin: epoch | 100, xmax: epoch | 200, xip: []uint64{epoch | 150}}
	for _, c := range []struct {
		xid  uint32
		full uint64
		vis  bool
	}{
		{50, epoch | 50, true},
		{150, epoch | 150, false}, // committed, still in xip (a synchronous-standby wait)
		{160, epoch | 160, true},
		{200, epoch | 200, false},
		{0xfffffff0, epoch - 0x10, true}, // from the epoch before xmax's
	} {
		full := s.extend(c.xid)
		if full != c.full || s.visible(full) != c.vis {
			t.Errorf("xid %d: full %d visible %v, want %d %v", c.xid, full, s.visible(full), c.full, c.vis)
		}
	}
	// Epoch 0 has no epoch before it.
	if got := (pgSnapshot{xmax: 10}).extend(20); got != 20 {
		t.Errorf("epoch 0: %d", got)
	}
}

// A transaction not yet visible holds its notifications and every later
// transaction's, messages included, in commit order.
func TestGateHoldsInvisible(t *testing.T) {
	const epoch = uint64(3) << 32
	var held atomic.Bool
	var snaps atomic.Int32
	g := newGate(func(context.Context) (pgSnapshot, error) {
		snaps.Add(1)
		s := pgSnapshot{xmin: epoch | 10, xmax: epoch | 20}
		if held.Load() {
			s.xip = []uint64{epoch | 11}
		}
		return s, nil
	}, nil)
	out := make(chan decodedTx, 10)
	g.deliver = func(tx decodedTx) { out <- tx }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.run(ctx) }()

	g.push(decodedTx{xid: 5, notes: []notification{{table: "a"}}})
	if tx := <-out; tx.notes[0].xid != epoch|5 {
		t.Fatalf("visible transaction: xid %d", tx.notes[0].xid)
	}
	held.Store(true)
	g.push(decodedTx{xid: 11, notes: []notification{{table: "held"}}})
	g.push(decodedTx{xid: 12, notes: []notification{{table: "later"}}})
	g.push(decodedTx{notes: []notification{{reval: "msg"}}})
	select {
	case tx := <-out:
		t.Fatalf("delivered %q while xid 11 is invisible", noteStrings(tx.notes))
	case <-time.After(50 * time.Millisecond):
	}
	if n := snaps.Load(); n < 3 {
		t.Errorf("polled %d times while holding", n)
	}
	held.Store(false)
	var order []string
	for range 3 {
		order = append(order, noteStrings((<-out).notes)...)
	}
	if want := []string{"held dirty", "later dirty", "reval msg"}; !slices.Equal(order, want) {
		t.Errorf("released %q, want %q", order, want)
	}
	cancel()
	<-done
}

// currentSnapshot against Postgres: a transaction in progress is not
// visible, and is once committed.
func TestCurrentSnapshotVisibility(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var xid uint64
	if err := tx.QueryRow(ctx, "SELECT txid_current()").Scan(&xid); err != nil {
		t.Fatal(err)
	}
	s, err := currentSnapshot(ctx, e.pool)
	if err != nil {
		t.Fatal(err)
	}
	if full := s.extend(uint32(xid)); full != xid || s.visible(full) {
		t.Errorf("in progress: extended %d (want %d), visible %v", full, xid, s.visible(full))
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if s, err = currentSnapshot(ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	if !s.visible(s.extend(uint32(xid))) {
		t.Error("committed transaction not visible")
	}
}

// canonical returns a notification's keys as sorted, re-marshalled JSON, so
// the triggers' "[1, \"a\"]" and the decoder's "[1,\"a\"]" compare equal.
func canonical(t *testing.T, keys string) []string {
	t.Helper()
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(keys), &arr); err != nil {
		t.Fatalf("%s: %v", keys, err)
	}
	var out []string
	for _, k := range arr {
		dec := json.NewDecoder(strings.NewReader(string(k)))
		dec.UseNumber()
		var v []any
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(v)
		out = append(out, string(b))
	}
	slices.Sort(out)
	return out
}

// The decoder encodes keys exactly as the triggers do, per key type, for
// the same writes: inserts, a non-key update, a key change and a delete.
func TestDecoderTriggerParity(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	type keyCase struct {
		name, cols, pk  string
		insert, setKey  string // VALUES list; SET clause changing the key
		where, whereDel string
	}
	cases := []keyCase{
		{"int2", "id int2, v int", "id", "(-3, 0), (7, 0)", "id = 8", "id = 7", "id = -3"},
		{"int4", "id int4, v int", "id", "(2147483647, 0), (1, 0)", "id = 2", "id = 1", "id = 2"},
		{"int8", "id int8, v int", "id", "(9007199254740993, 0), (-1, 0)", "id = 5", "id = -1", "id = 5"},
		{"text", "id text, v int", "id", `('a"b\c é', 0), ('x', 0)`, "id = 'y'", "id = 'x'", `id = 'a"b\c é'`},
		{"uuid", "id uuid, v int", "id", "('6f1c2a3e-0000-4000-8000-00000000000a', 0), ('6f1c2a3e-0000-4000-8000-00000000000b', 0)",
			"id = '6f1c2a3e-0000-4000-8000-00000000000c'", "id = '6f1c2a3e-0000-4000-8000-00000000000b'", "id = '6f1c2a3e-0000-4000-8000-00000000000a'"},
		// Declared (id, org) against column order (org, id).
		{"composite", "org text, id int4, v int", "id, org", "('acme', 1, 0), ('globex', 1, 0)", "org = 'initech'", "org = 'globex'", "org = 'acme'"},
	}
	var tables []string
	for _, c := range cases {
		tb := e.table + "_k" + c.name
		tables = append(tables, tb)
		execAll(t, e, fmt.Sprintf("CREATE TABLE %s (%s, PRIMARY KEY (%s))", tb, c.cols, c.pk))
		t.Cleanup(func() { e.pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+tb) })
	}
	if err := InstallTriggers(ctx, e.pool, tables...); err != nil {
		t.Fatal(err)
	}
	if err := InstallPublication(ctx, e.pool, tables...); err != nil {
		t.Fatal(err)
	}

	listen, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer listen.Release()
	if _, err := listen.Exec(ctx, "LISTEN comet"); err != nil {
		t.Fatal(err)
	}
	slotConn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer slotConn.Release()
	slot := "cometgo_parity_" + e.table
	if _, err := slotConn.Exec(ctx, "SELECT pg_create_logical_replication_slot($1, 'pgoutput', true)", slot); err != nil {
		t.Fatal(err)
	}

	// One statement per transaction: one notification per write either way.
	var stmts []string
	for i, c := range cases {
		tb := tables[i]
		stmts = append(stmts,
			fmt.Sprintf("INSERT INTO %s VALUES %s", tb, c.insert),
			fmt.Sprintf("UPDATE %s SET v = v + 1", tb),
			fmt.Sprintf("UPDATE %s SET %s WHERE %s", tb, c.setKey, c.where),
			fmt.Sprintf("DELETE FROM %s WHERE %s", tb, c.whereDel))
	}
	for _, s := range stmts {
		execAll(t, e, s)
	}

	var trig [][]string
	for range stmts {
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		n, err := listen.Conn().WaitForNotification(wctx)
		cancel()
		if err != nil {
			t.Fatalf("trigger notifications: got %d of %d: %v", len(trig), len(stmts), err)
		}
		p := parseNotification(n.Payload)
		trig = append(trig, append([]string{p.table}, canonical(t, p.keys)...))
	}

	rows, err := slotConn.Query(ctx, `
		SELECT data FROM pg_logical_slot_get_binary_changes($1, NULL, NULL,
			'proto_version', '1', 'publication_names', 'cometgo', 'messages', 'true')`, slot)
	if err != nil {
		t.Fatal(err)
	}
	var raws [][]byte
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		raws = append(raws, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	d := newDecoder(func(ctx context.Context, rel uint32) ([]string, error) { return pkColumns(ctx, e.pool, rel) })
	var logical [][]string
	for _, b := range raws {
		m, err := pglogrepl.Parse(b)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := d.handle(ctx, m)
		if err != nil {
			t.Fatal(err)
		}
		if tx == nil {
			continue
		}
		for _, n := range tx.notes {
			logical = append(logical, append([]string{n.table}, canonical(t, n.keys)...))
		}
	}
	if len(logical) != len(trig) {
		t.Fatalf("decoder gave %d notifications, triggers %d", len(logical), len(trig))
	}
	for i := range trig {
		if !slices.Equal(logical[i], trig[i]) {
			t.Errorf("%s\n  triggers %q\n  decoder  %q", stmts[i], trig[i], logical[i])
		}
	}
}

// A snapshot error after part of the queue went out keeps only the rest:
// nothing is delivered twice.
func TestGateErrorDeliversOnce(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	g := newGate(func(context.Context) (pgSnapshot, error) {
		if fail.Load() {
			return pgSnapshot{}, errors.New("connection lost")
		}
		return pgSnapshot{xmin: 100, xmax: 100}, nil
	}, nil)
	var got []string
	g.deliver = func(tx decodedTx) { got = append(got, noteStrings(tx.notes)...) }
	g.push(decodedTx{notes: []notification{{reval: "msg"}}})
	g.push(decodedTx{xid: 11, notes: []notification{{table: "t"}}})
	if err := g.run(context.Background()); err == nil {
		t.Fatal("run returned no error")
	}
	fail.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	g.run(ctx)
	if want := []string{"reval msg", "t dirty"}; !slices.Equal(got, want) {
		t.Errorf("delivered %q, want %q", got, want)
	}
}

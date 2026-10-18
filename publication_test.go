package cometgo

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// checkLogical runs the logical-mode table checks Start will run, for one
// publication per table.
func checkLogical(e *testEnv, tables ...string) error {
	app := New(Config{DB: e.pool, Feed: FeedLogical})
	for i, tb := range tables {
		Publish(app, fmt.Sprint("p", i), PubOptions{Tables: []string{tb}}, func(*SubCtx, struct{}) (Query, error) { return Query{}, nil })
	}
	return app.checkTables(context.Background())
}

func execAll(t *testing.T, e *testEnv, qs ...string) {
	t.Helper()
	for _, q := range qs {
		if _, err := e.pool.Exec(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// pubCols returns the table's column list in the publication, nil if it
// is not a member.
func pubCols(t *testing.T, e *testEnv, table string) []string {
	t.Helper()
	var cols []string
	err := e.pool.QueryRow(context.Background(), `
		SELECT coalesce(attnames, '{}') FROM pg_publication_tables
		WHERE pubname = 'cometgo' AND schemaname || '.' || tablename = $1`, qualifyTable(table)).Scan(&cols)
	if err != nil {
		return nil
	}
	return cols
}

func TestInstallPublicationIdempotent(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	all := []string{"id", "owner", "name", "n"}
	for range 2 {
		if err := InstallPublication(ctx, e.pool, e.table); err != nil {
			t.Fatal(err)
		}
		if got := pubCols(t, e, e.table); !slices.Equal(got, all) {
			t.Fatalf("whole rows: columns %v, want %v", got, all)
		}
	}
	for range 2 {
		if err := InstallPublicationKeysOnly(ctx, e.pool, e.table); err != nil {
			t.Fatal(err)
		}
		if got := pubCols(t, e, e.table); !slices.Equal(got, []string{"id"}) {
			t.Fatalf("keys only: columns %v, want [id]", got)
		}
	}
	if err := checkLogical(e, e.table); err != nil {
		t.Fatalf("keys only: %v", err)
	}
	if err := InstallPublication(ctx, e.pool, e.table); err != nil {
		t.Fatal(err)
	}
	if got := pubCols(t, e, e.table); !slices.Equal(got, all) {
		t.Fatalf("back to whole rows: columns %v, want %v", got, all)
	}
	if err := checkLogical(e, e.table); err != nil {
		t.Fatalf("whole rows: %v", err)
	}
}

// InstallPublication refuses a table whose UPDATEs Postgres would then
// refuse, rather than break the app's writes.
func TestInstallPublicationRefusesUnusableIdentity(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	nopk, full := e.table+"_nopk", e.table+"_full"
	execAll(t, e,
		"CREATE TABLE "+nopk+" (a int)",
		// The generated column is not published, and FULL needs no list.
		"CREATE TABLE "+full+" (id int PRIMARY KEY, a int, g int GENERATED ALWAYS AS (a * 2) STORED)",
		"ALTER TABLE "+full+" REPLICA IDENTITY FULL")
	t.Cleanup(func() { e.pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+nopk+", "+full) })
	if err := InstallPublication(ctx, e.pool, nopk); err == nil || !strings.Contains(err.Error(), nopk) || !strings.Contains(err.Error(), "no primary key") {
		t.Errorf("no primary key: %v", err)
	}
	if pubCols(t, e, nopk) != nil {
		t.Error("table without a primary key was published")
	}
	if err := InstallPublication(ctx, e.pool, full); err != nil {
		t.Errorf("FULL: %v", err)
	}
	if err := checkLogical(e, full); err != nil {
		t.Errorf("FULL with a generated column: %v", err)
	}
	if err := InstallPublicationKeysOnly(ctx, e.pool, full); err == nil || !strings.Contains(err.Error(), "REPLICA IDENTITY FULL") {
		t.Errorf("FULL keys only: %v", err)
	}
}

// Each logical-mode check fails at Start with a message naming the table.
func TestCheckTablesLogical(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	unpub, nothing, dropped, idx, moved := e.table+"_unpub", e.table+"_nothing", e.table+"_dropped", e.table+"_idx", e.table+"_moved"
	for _, tb := range []string{unpub, nothing, dropped, idx, moved} {
		execAll(t, e, "CREATE TABLE "+tb+" (id int PRIMARY KEY, k int NOT NULL)")
		t.Cleanup(func() { e.pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+tb) })
	}
	if err := InstallPublication(ctx, e.pool, nothing, dropped, idx); err != nil {
		t.Fatal(err)
	}
	if err := InstallPublicationKeysOnly(ctx, e.pool, moved); err != nil {
		t.Fatal(err)
	}
	execAll(t, e,
		"ALTER TABLE "+nothing+" REPLICA IDENTITY NOTHING",
		"ALTER TABLE "+dropped+" DROP CONSTRAINT "+dropped+"_pkey",
		"CREATE UNIQUE INDEX "+idx+"_k ON "+idx+" (k)",
		"ALTER TABLE "+idx+" REPLICA IDENTITY USING INDEX "+idx+"_k",
		// The key changes after the key-only list was made for it.
		"ALTER TABLE "+moved+" DROP CONSTRAINT "+moved+"_pkey",
		"ALTER TABLE "+moved+" ADD PRIMARY KEY (k)")
	for _, c := range []struct{ table, want string }{
		{unpub, `not in publication "cometgo"`},
		{nothing, "REPLICA IDENTITY NOTHING"},
		{dropped, "no primary key"},
		{idx, "REPLICA IDENTITY USING INDEX"},
		{moved, "does not cover the replica identity of " + moved + " (missing k)"},
	} {
		err := checkLogical(e, c.table)
		if err == nil || !strings.Contains(err.Error(), "table public."+c.table+":") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q naming the table", c.table, err, c.want)
		}
	}
	// The notify feed still checks triggers, not the publication.
	app := New(Config{DB: e.pool})
	Publish(app, "p", PubOptions{Tables: []string{unpub}}, func(*SubCtx, struct{}) (Query, error) { return Query{}, nil })
	if err := app.checkTables(ctx); err == nil || !strings.Contains(err.Error(), "missing or disabled triggers") {
		t.Errorf("notify mode: %v", err)
	}
}

func TestCheckPublicationSettings(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	if err := InstallPublication(ctx, e.pool, e.table); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { InstallPublication(context.Background(), e.pool) })
	execAll(t, e, "ALTER PUBLICATION cometgo SET (publish_via_partition_root = false)")
	if err := checkLogical(e, e.table); err == nil || !strings.Contains(err.Error(), "via the partition root") {
		t.Errorf("publish_via_partition_root off: %v", err)
	}
	if err := InstallPublication(ctx, e.pool); err != nil {
		t.Fatal(err)
	}
	if err := checkLogical(e, e.table); err != nil {
		t.Errorf("restored: %v", err)
	}
}

// A partitioned table is checked by its leaves' identity, and its changes
// are decoded under the parent's name, as Tables lists it.
func TestPartitionedTablePublishedUnderRoot(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	parent := e.table + "_part"
	execAll(t, e,
		"CREATE TABLE "+parent+" (id int PRIMARY KEY, v text) PARTITION BY RANGE (id)",
		"CREATE TABLE "+parent+"_a PARTITION OF "+parent+" FOR VALUES FROM (0) TO (100)",
		"CREATE TABLE "+parent+"_b PARTITION OF "+parent+" FOR VALUES FROM (100) TO (200)")
	t.Cleanup(func() { e.pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+parent) })
	if err := InstallPublication(ctx, e.pool, parent); err != nil {
		t.Fatal(err)
	}
	if err := checkLogical(e, parent); err != nil {
		t.Fatalf("partitioned: %v", err)
	}
	execAll(t, e, "ALTER TABLE "+parent+"_b REPLICA IDENTITY NOTHING")
	if err := checkLogical(e, parent); err == nil || !strings.Contains(err.Error(), parent+"_b has REPLICA IDENTITY NOTHING") {
		t.Errorf("leaf with no identity: %v", err)
	}
	execAll(t, e, "ALTER TABLE "+parent+"_b REPLICA IDENTITY DEFAULT")

	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	slot := "cometgo_test_" + e.table
	if _, err := conn.Exec(ctx, "SELECT pg_create_logical_replication_slot($1, 'pgoutput', true)", slot); err != nil {
		t.Fatal(err)
	}
	execAll(t, e, "INSERT INTO "+parent+" VALUES (1, 'a'), (150, 'b')")
	rows, err := conn.Query(ctx, `
		SELECT data FROM pg_logical_slot_peek_binary_changes($1, NULL, NULL,
			'proto_version', '1', 'publication_names', 'cometgo')`, slot)
	if err != nil {
		t.Fatal(err)
	}
	// Inserts name their relation by oid, from a Relation message: pgoutput
	// sends one for the leaf too, but a change must point at the root.
	rels := map[uint32]string{}
	var inserted []string
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			t.Fatal(err)
		}
		switch data[0] {
		case 'R': // Relation: 'R', int32 oid, namespace\0, name\0, ...
			parts := bytes.SplitN(data[5:], []byte{0}, 3)
			rels[binary.BigEndian.Uint32(data[1:5])] = string(parts[0]) + "." + string(parts[1])
		case 'I': // Insert: 'I', int32 oid, ...
			inserted = append(inserted, rels[binary.BigEndian.Uint32(data[1:5])])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := "public." + parent
	if !slices.Equal(inserted, []string{want, want}) {
		t.Errorf("inserts decoded under %v, want both under %s", inserted, want)
	}
}

func TestUninstallTriggers(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	renamed := e.table + "_renamed"
	execAll(t, e, "CREATE TABLE "+e.table+"_old (id int PRIMARY KEY)")
	t.Cleanup(func() { e.pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+e.table+"_old, "+renamed) })
	if err := InstallTriggers(ctx, e.pool, e.table+"_old"); err != nil {
		t.Fatal(err)
	}
	// Installed under its old name: its functions carry that name.
	execAll(t, e, "ALTER TABLE "+e.table+"_old RENAME TO "+renamed)
	funcs := func(table string) int {
		var n int
		if err := e.pool.QueryRow(ctx, "SELECT count(*) FROM pg_proc WHERE proname LIKE $1", triggerFuncPrefix("public."+table)+"\\_%").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if funcs(e.table) != 4 || funcs(e.table+"_old") != 4 {
		t.Fatalf("before: %d and %d trigger functions", funcs(e.table), funcs(e.table+"_old"))
	}
	for range 2 {
		if err := UninstallTriggers(ctx, e.pool, e.table, renamed); err != nil {
			t.Fatal(err)
		}
	}
	if n, m := funcs(e.table), funcs(e.table+"_old"); n != 0 || m != 0 {
		t.Errorf("after: %d and %d trigger functions left", n, m)
	}
	var trigs int
	if err := e.pool.QueryRow(ctx, "SELECT count(*) FROM pg_trigger WHERE tgrelid IN ($1::regclass, $2::regclass) AND tgname LIKE 'cometgo\\_%'", e.table, renamed).Scan(&trigs); err != nil {
		t.Fatal(err)
	}
	if trigs != 0 {
		t.Errorf("%d triggers left", trigs)
	}
	// A notify-mode start now fails, as a rollback after UninstallTriggers must.
	app := New(Config{DB: e.pool})
	Publish(app, "p", PubOptions{Tables: []string{e.table}}, func(*SubCtx, struct{}) (Query, error) { return Query{}, nil })
	if err := app.checkTables(ctx); err == nil || !strings.Contains(err.Error(), "missing or disabled triggers") {
		t.Errorf("notify start after uninstall: %v", err)
	}
	if err := UninstallTriggers(ctx, e.pool, "nope_"+e.table); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("missing table: %v", err)
	}
}

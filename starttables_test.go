package cometgo

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Start refuses Tables entries the change feed would never match.
func TestStartChecksTables(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	bare, other := e.table+"_bare", "s_"+e.table
	for _, q := range []string{
		fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY)", bare),
		"CREATE SCHEMA " + other,
		fmt.Sprintf("CREATE TABLE %s.t (id int PRIMARY KEY)", other),
	} {
		if _, err := e.pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		e.pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+bare)
		e.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+other+" CASCADE")
	})
	if err := InstallTriggers(ctx, e.pool, other+".t"); err != nil {
		t.Fatal(err)
	}

	start := func(tables ...string) error {
		app := New(Config{DB: e.pool})
		for i, tb := range tables {
			Publish(app, fmt.Sprint("p", i), PubOptions{Tables: []string{tb}}, func(*SubCtx, struct{}) (Query, error) { return Query{}, nil })
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		return app.Start(ctx)
	}
	for _, c := range []struct{ table, want string }{
		{"nope_" + e.table, "not found"},
		{bare, "missing or disabled triggers cometgo_ins, cometgo_upd, cometgo_del, cometgo_trunc"},
		{strings.ToUpper(e.table), fmt.Sprintf(`did you mean "public.%s"`, e.table)},
		{"t", "public.t: not found"}, // other.t is not reached through search_path
	} {
		if err := start(c.table); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Tables %q: got %v, want %q", c.table, err, c.want)
		}
	}
	// Every bad table is reported at once.
	if err := start("nope_"+e.table, bare); err == nil || !strings.Contains(err.Error(), "p0") || !strings.Contains(err.Error(), "p1") {
		t.Errorf("both bad tables: %v", err)
	}
	if err := start(e.table, other+".t"); err != nil {
		t.Errorf("valid tables: %v", err)
	}

	// Disabled and replica-only triggers don't fire, so count as missing.
	for _, q := range []string{"DISABLE TRIGGER cometgo_upd", "ENABLE REPLICA TRIGGER cometgo_del"} {
		if _, err := e.pool.Exec(ctx, "ALTER TABLE "+other+".t "+q); err != nil {
			t.Fatal(err)
		}
	}
	if err := start(other + ".t"); err == nil || !strings.Contains(err.Error(), "triggers cometgo_upd, cometgo_del;") {
		t.Errorf("disabled trigger: %v", err)
	}

	// Moved after InstallTriggers: the triggers move with the table but
	// still notify under the old name.
	if err := InstallTriggers(ctx, e.pool, bare); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, "ALTER TABLE "+bare+" SET SCHEMA "+other); err != nil {
		t.Fatal(err)
	}
	if err := start(other + "." + bare); err == nil || !strings.Contains(err.Error(), "installed for another table name") {
		t.Errorf("moved table: %v", err)
	}
	if err := InstallTriggers(ctx, e.pool, other+"."+bare); err != nil {
		t.Fatal(err)
	}
	if err := start(other + "." + bare); err != nil {
		t.Errorf("moved table after reinstall: %v", err)
	}
}

// Start panics when called again after succeeding, and after Shutdown it
// does nothing and returns nil.
func TestStartTwiceAndAfterShutdown(t *testing.T) {
	e := setup(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Error("second Start did not panic")
			}
		}()
		e.app.Start(context.Background())
	}()

	app := New(Config{DB: e.pool})
	Publish(app, "p", PubOptions{Tables: []string{"nope_" + e.table}}, func(*SubCtx, struct{}) (Query, error) { return Query{}, nil })
	if err := app.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := app.Start(context.Background()); err != nil {
		t.Errorf("Start after Shutdown: %v", err)
	}
}

// With the database unreachable, Start fails instead of starting an engine.
func TestStartFailsWithoutDatabase(t *testing.T) {
	e := setup(t)
	pool, err := pgxpool.New(context.Background(), "postgres://x:x@127.0.0.1:1/x?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	app := New(Config{DB: pool, QueryTimeout: 2 * time.Second})
	Publish(app, "p", PubOptions{Tables: []string{e.table}}, func(*SubCtx, struct{}) (Query, error) { return Query{}, nil })
	if err := app.Start(context.Background()); err == nil {
		t.Fatal("Start succeeded without a database")
	}
}

// A failed Start starts nothing, so the app can Start again once fixed.
func TestStartRetryAfterFix(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	late := e.table + "_late"
	t.Cleanup(func() { e.pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+late) })
	app := New(Config{DB: e.pool})
	Publish(app, "late", PubOptions{Tables: []string{late}}, func(*SubCtx, struct{}) (Query, error) { return Query{}, nil })
	if err := app.Start(ctx); err == nil {
		t.Fatal("Start succeeded without the table")
	}
	if _, err := e.pool.Exec(ctx, "CREATE TABLE "+late+" (id int PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := InstallTriggers(ctx, e.pool, late); err != nil {
		t.Fatal(err)
	}
	if err := app.Start(ctx); err != nil {
		t.Fatal(err)
	}
}

// New refuses a pool of one connection, where a tick resolving column types
// would wait forever for a second one.
func TestNewRejectsSingleConnPool(t *testing.T) {
	pool, err := pgxpool.New(context.Background(), "postgres://x:x@127.0.0.1:1/x?pool_max_conns=1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	defer func() {
		if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "MaxConns >= 2") {
			t.Fatalf("New with MaxConns=1: recovered %v, want MaxConns panic", r)
		}
	}()
	New(Config{DB: pool})
}

// Start refuses a feed that is not implemented, and an unknown one.
func TestStartChecksFeed(t *testing.T) {
	e := setup(t)
	for _, c := range []struct {
		feed Feed
		want string
	}{
		{FeedLogical, "not implemented yet"},
		{Feed(7), "unknown feed 7"},
	} {
		app := New(Config{DB: e.pool, Feed: c.feed})
		if err := app.Start(context.Background()); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Feed %d: got %v, want %q", c.feed, err, c.want)
		}
	}
}

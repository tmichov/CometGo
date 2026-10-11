package cometgo

import (
	"context"
	"fmt"
	"strings"
	"testing"
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

	// A disabled trigger counts as missing.
	if _, err := e.pool.Exec(ctx, "ALTER TABLE "+other+".t DISABLE TRIGGER cometgo_upd"); err != nil {
		t.Fatal(err)
	}
	if err := start(other + ".t"); err == nil || !strings.Contains(err.Error(), "triggers cometgo_upd;") {
		t.Errorf("disabled trigger: %v", err)
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

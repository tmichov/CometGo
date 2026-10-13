package cometgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestParseArrayLiteral(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{`{}`, `[]`},
		{`{happy,sad}`, `["happy","sad"]`},
		{`{"it's complicated",NULL,"NULL","a\"b\\c","x,y"}`, `["it's complicated",null,"NULL","a\"b\\c","x,y"]`},
		{`{{a,b},{c,d}}`, `[["a","b"],["c","d"]]`},
		{`[0:1]={a,b}`, `["a","b"]`},
	} {
		got, err := encodeEnumArray(c.in)
		if err != nil || string(got) != c.want {
			t.Errorf("%s: %s, %v; want %s", c.in, got, err, c.want)
		}
	}
	// Escaped like every other string cell (quote).
	if got, _ := encodeEnumArray(`{<b>}`); string(got) != "["+string(quote("<b>"))+"]" {
		t.Errorf("escaping: %s", got)
	}
	for _, bad := range []string{``, `{`, `{a`, `{"a}`, `{a}x`, `[0:1]`} {
		if _, err := parseArrayLiteral(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// User-defined enums, unknown to pgx, are reported as enum (and arrays of
// them, or of a domain over one, as []enum), with values as strings.
func TestUserEnumTypeCode(t *testing.T) {
	var table, mood string
	e := setupFull(t, nil, func(e *testEnv, app *App) {
		table, mood = e.table+"_m", "mood_"+e.table[len("items_"):]
		for _, ddl := range []string{
			fmt.Sprintf("CREATE TYPE %s AS ENUM ('happy', 'sad', 'it''s complicated')", mood),
			fmt.Sprintf("CREATE DOMAIN %s_d AS %s", mood, mood),
			fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, m %s, ms %s[], md %s_d, mds %s_d[], note text)", table, mood, mood, mood, mood),
		} {
			if _, err := e.pool.Exec(context.Background(), ddl); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() {
			e.pool.Exec(context.Background(), "DROP TABLE "+table)
			e.pool.Exec(context.Background(), "DROP DOMAIN "+mood+"_d")
			e.pool.Exec(context.Background(), "DROP TYPE "+mood)
		})
		if err := InstallTriggers(context.Background(), e.pool, table); err != nil {
			t.Fatal(err)
		}
		Publish(app, "moods", PubOptions{Tables: []string{table}, Scope: ScopeShared},
			func(c *SubCtx, _ struct{}) (Query, error) {
				return Query{SQL: "SELECT id, m, ms, md, mds, note FROM " + table + " ORDER BY id"}, nil
			})
	})
	if _, err := e.pool.Exec(context.Background(), fmt.Sprintf(
		"INSERT INTO %s VALUES (1, 'happy', ARRAY['sad', 'it''s complicated']::%[2]s[], 'sad', ARRAY['happy']::%[2]s_d[], 'x'), (2, NULL, '{}', NULL, NULL, NULL)", table, mood)); err != nil {
		t.Fatal(err)
	}
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "moods"})
	en := entries(a.next())[0]
	if en["end"] != nil {
		t.Fatalf("ended: %v", en["end"])
	}
	cols := fmt.Sprint(en["init"].(map[string]any)["cols"])
	if want := "[[id i4] [m enum] [ms []enum] [md enum] [mds []enum] [note text]]"; cols != want {
		t.Fatalf("cols %s, want %s", cols, want)
	}
	ops, _ := json.Marshal(en["ops"])
	if want := `[["i","1",[1,"happy",["sad","it's complicated"],"sad",["happy"],"x"],0],["i","2",[2,null,[],null,null,null],1]]`; string(ops) != want {
		t.Fatalf("ops %s\nwant %s", ops, want)
	}
	for i, m := range []string{"sad", "it''s complicated"} {
		if _, err := e.pool.Exec(context.Background(), fmt.Sprintf("UPDATE %s SET m = '%s', ms = ms || '{happy}' WHERE id = 1", table, m)); err != nil {
			t.Fatal(err)
		}
		ops, _ := json.Marshal(entries(a.next())[0]["ops"])
		want := []string{
			`[["u","1",[1,"sad",2,["sad","it's complicated","happy"]]]]`,
			`[["u","1",[1,"it's complicated",2,["sad","it's complicated","happy","happy"]]]]`,
		}[i]
		if string(ops) != want {
			t.Fatalf("patch %s\nwant  %s", ops, want)
		}
	}
	// Nothing is registered on the pool's connections: the app's own
	// queries still get an unknown array type as its text literal.
	for range 4 {
		var lit string
		if err := e.pool.QueryRow(context.Background(), "SELECT ms FROM "+table+" WHERE id = 1").Scan(&lit); err != nil || lit[0] != '{' {
			t.Fatalf("app query got %q, %v", lit, err)
		}
	}
}

// A type created after the server started (a migration while it runs) is
// looked up when first seen, not missed for good. The table must exist at
// Start, so the migration changes its column to the new type.
func TestUserEnumCreatedWhileRunning(t *testing.T) {
	var table, mood string
	e := setupFull(t, nil, func(e *testEnv, app *App) {
		table, mood = e.table+"_late", "late_"+e.table[len("items_"):]
		t.Cleanup(func() {
			e.pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
			e.pool.Exec(context.Background(), "DROP TYPE IF EXISTS "+mood)
		})
		if _, err := e.pool.Exec(context.Background(), fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, m text)", table)); err != nil {
			t.Fatal(err)
		}
		if err := InstallTriggers(context.Background(), e.pool, table); err != nil {
			t.Fatal(err)
		}
		Publish(app, "late", PubOptions{Tables: []string{table}, Scope: ScopeShared},
			func(c *SubCtx, _ struct{}) (Query, error) {
				return Query{SQL: "SELECT id, m FROM " + table + " ORDER BY id"}, nil
			})
	})
	for _, ddl := range []string{
		fmt.Sprintf("CREATE TYPE %s AS ENUM ('on', 'off')", mood),
		fmt.Sprintf("ALTER TABLE %s ALTER COLUMN m TYPE %s USING m::%s", table, mood, mood),
		fmt.Sprintf("INSERT INTO %s VALUES (1, 'on')", table),
	} {
		if _, err := e.pool.Exec(context.Background(), ddl); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "late"})
	en := entries(a.next())[0]
	if cols := fmt.Sprint(en["init"].(map[string]any)["cols"]); cols != "[[id i4] [m enum]]" {
		t.Fatalf("cols %s", cols)
	}
}

// The type lookup takes a second connection while its caller holds one. With
// the rest of the pool held elsewhere it fails after QueryTimeout instead of
// waiting forever: Describe returns the error, and a tick retries without
// ending the sub, which gets its snapshot once a connection frees.
func TestTypeLookupBoundedWhenPoolExhausted(t *testing.T) {
	const timeout = 300 * time.Millisecond
	logs := &errLogs{}
	var pool *pgxpool.Pool
	var table string
	e := setupFull(t, func(c *Config) {
		cfg, err := pgxpool.ParseConfig(os.Getenv("COMETGO_TEST_DATABASE_URL"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.MaxConns = 2
		if pool, err = pgxpool.NewWithConfig(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		c.DB, c.QueryTimeout, c.Logger = pool, timeout, slog.New(logs)
	}, func(e *testEnv, app *App) {
		table = e.table + "_m"
		mood := "mood_" + e.table[len("items_"):]
		for _, ddl := range []string{
			fmt.Sprintf("CREATE TYPE %s AS ENUM ('happy', 'sad')", mood),
			fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, m %s)", table, mood),
			fmt.Sprintf("INSERT INTO %s VALUES (1, 'happy')", table),
		} {
			if _, err := e.pool.Exec(context.Background(), ddl); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() {
			e.pool.Exec(context.Background(), "DROP TABLE "+table)
			e.pool.Exec(context.Background(), "DROP TYPE "+mood)
		})
		if err := InstallTriggers(context.Background(), e.pool, table); err != nil {
			t.Fatal(err)
		}
		Publish(app, "moods", PubOptions{Tables: []string{table}, Scope: ScopeShared},
			func(c *SubCtx, _ struct{}) (Query, error) {
				return Query{SQL: "SELECT id, m FROM " + table + " ORDER BY id"}, nil
			})
	})
	held, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if held != nil {
			held.Release()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	_, err = Describe(ctx, e.app, DescribeOptions{Identity: &Identity{UserID: "u"}})
	if !errors.Is(err, errTypeLookup) || time.Since(start) > 4*timeout {
		t.Fatalf("Describe: %v after %v, want a type lookup error after ~%v", err, time.Since(start), timeout)
	}

	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "moods"})
	deadline := time.Now().Add(5 * time.Second)
	tickFailed := func() bool {
		logs.mu.Lock()
		defer logs.mu.Unlock()
		return slices.ContainsFunc(logs.errs, func(s string) bool { return strings.Contains(s, "look up column types") })
	}
	for !tickFailed() {
		if time.Now().After(deadline) {
			t.Fatal("tick never failed its type lookup")
		}
		time.Sleep(20 * time.Millisecond)
	}
	held.Release()
	held = nil
	en := entries(a.next())[0]
	if en["end"] != nil {
		t.Fatalf("sub ended: %v", en["end"])
	}
	if ops, _ := json.Marshal(en["ops"]); string(ops) != `[["i","1",[1,"happy"],0]]` {
		t.Fatalf("ops %s", ops)
	}
}

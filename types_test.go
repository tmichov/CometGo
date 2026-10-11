package cometgo

import (
	"context"
	"fmt"
	"testing"
)

// User-defined enums, unknown to pgx, are reported as enum (and arrays of
// them as []enum), with values as strings.
func TestUserEnumTypeCode(t *testing.T) {
	var table, mood string
	e := setupFull(t, nil, func(e *testEnv, app *App) {
		table, mood = e.table+"_m", "mood_"+e.table[len("items_"):]
		for _, ddl := range []string{
			fmt.Sprintf("CREATE TYPE %s AS ENUM ('happy', 'sad', 'it''s complicated')", mood),
			fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, m %s, ms %s[], note text)", table, mood, mood),
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
				return Query{SQL: "SELECT id, m, ms, note FROM " + table + " ORDER BY id"}, nil
			})
	})
	if _, err := e.pool.Exec(context.Background(), fmt.Sprintf(
		"INSERT INTO %s VALUES (1, 'happy', ARRAY['sad', 'it''s complicated']::%s[], 'x'), (2, NULL, '{}', NULL)", table, mood)); err != nil {
		t.Fatal(err)
	}
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "moods"})
	en := entries(a.next())[0]
	if en["end"] != nil {
		t.Fatalf("ended: %v", en["end"])
	}
	cols := fmt.Sprint(en["init"].(map[string]any)["cols"])
	if want := "[[id i4] [m enum] [ms []enum] [note text]]"; cols != want {
		t.Fatalf("cols %s, want %s", cols, want)
	}
	ops := en["ops"].([]any)
	if got := fmt.Sprint(ops[0].([]any)[2], ops[1].([]any)[2]); got != "[1 happy [sad it's complicated] x] [2 <nil> [] <nil>]" {
		t.Fatalf("rows %s", got)
	}
	// Live diffs decode the same way, on whatever pooled connection runs them.
	for i := range 3 {
		if _, err := e.pool.Exec(context.Background(), fmt.Sprintf("UPDATE %s SET m = '%s' WHERE id = 1", table, []string{"sad", "happy", "it''s complicated"}[i])); err != nil {
			t.Fatal(err)
		}
		m := a.next()
		ops := entries(m)[0]["ops"].([]any)
		if got := fmt.Sprint(ops[0].([]any)[2]); got != fmt.Sprintf("[1 %s]", []string{"sad", "happy", "it's complicated"}[i]) {
			t.Fatalf("patch %s", got)
		}
	}
}

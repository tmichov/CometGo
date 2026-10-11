package cometgo

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// fallbacks counts "filter unavailable" logs: the filter giving up and
// re-running every candidate.
type fallbacks struct{ n atomic.Int32 }

func (f *fallbacks) Enabled(context.Context, slog.Level) bool { return true }
func (f *fallbacks) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, "filter unavailable") {
		f.n.Add(1)
	}
	return nil
}
func (f *fallbacks) WithAttrs([]slog.Attr) slog.Handler { return f }
func (f *fallbacks) WithGroup(string) slog.Handler      { return f }

// subRows mirrors each sub's rows by key, from its ops.
type subRows map[float64]map[string][]any

func (m subRows) apply(t testing.TB, msg map[string]any) {
	for _, e := range entries(msg) {
		s := e["s"].(float64)
		if e["init"] != nil || m[s] == nil {
			m[s] = map[string][]any{}
		}
		ops, _ := e["ops"].([]any)
		for _, o := range ops {
			o := o.([]any)
			key := o[1].(string)
			switch o[0] {
			case "i":
				m[s][key] = o[2].([]any)
			case "u":
				row := m[s][key]
				if row == nil {
					t.Fatalf("sub %v: patch of unknown key %s", s, key)
				}
				patch := o[2].([]any)
				for i := 0; i < len(patch); i += 2 {
					row[int(patch[i].(float64))] = patch[i+1]
				}
			case "d":
				delete(m[s], key)
			}
		}
	}
}

func TestCanonKeysMatchRowKey(t *testing.T) {
	// Single keys: rowKey is the unquoted wire string.
	canon, texts, ok := canonKeys([]json.RawMessage{[]byte(`[42]`), []byte(`["7"]`), []byte(`["a\"b<"]`)}, 1)
	if !ok {
		t.Fatal("not ok")
	}
	for i, cell := range []string{`42`, `"7"`, `"a\"b<"`} {
		want, _ := rowKey([]json.RawMessage{json.RawMessage(cell)}, []int{0})
		if canon[i] != want {
			t.Fatalf("key %d: %q, rowKey %q", i, canon[i], want)
		}
	}
	if strings.Join(texts[0], ",") != `42,7,a"b<` {
		t.Fatalf("texts %v", texts)
	}
	// Composite: compact JSON of the wire cells, as jsonb prints them with spaces.
	canon, _, ok = canonKeys([]json.RawMessage{[]byte(`["org<1>", 42]`)}, 2)
	want, _ := rowKey([]json.RawMessage{quote("org<1>"), json.RawMessage(`42`)}, []int{0, 1})
	if !ok || canon[0] != want {
		t.Fatalf("composite %q, rowKey %q", canon, want)
	}
	for _, bad := range []string{`[null]`, `[true]`, `[[1]]`, `[1,2]`, `x`} {
		if _, _, ok := canonKeys([]json.RawMessage{[]byte(bad)}, 1); ok {
			t.Fatalf("%s accepted", bad)
		}
	}
}

func TestSQLEqual(t *testing.T) {
	u := [16]byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0, 1, 2, 3, 4, 5, 6, 7, 8}
	for _, c := range []struct {
		got, want any
		eq, ok    bool
	}{
		{"a", "a", true, true},
		{"a", "b", false, true},
		{int32(5), 5, true, true},
		{int64(5), int16(6), false, true},
		{true, true, true, true},
		{nil, "a", false, true}, // NULL = x is not true
		{u, "12345678-9abc-def0-0102-030405060708", true, true},
		{u, u, true, true},
		{u, "not-a-uuid", false, false},
		{int32(7), uint8(7), true, true},
		{int64(7), uint64(1 << 63), false, false},
		{"5", 5, false, false},   // no implicit casts: re-run
		{1.5, 1.5, false, false}, // floats are not compared
		{"a", nil, false, false},
	} {
		eq, ok := sqlEqual(c.got, c.want)
		if eq != c.eq || ok != c.ok {
			t.Errorf("sqlEqual(%v, %v) = %v, %v", c.got, c.want, eq, ok)
		}
	}
}

// setupFilter registers, for the env's table and two more (a composite and
// a uuid primary key), pairs of publications that differ only in Match:
// "f.*" declares it, "u.*" does not. "exec" runs SQL statements in one
// transaction.
func setupFilter(t testing.TB, tweak func(*Config)) *testEnv {
	t.Helper()
	return setupFull(t, tweak, func(e *testEnv, app *App) {
		ctx := context.Background()
		comp, uu := e.table+"_c", e.table+"_u"
		for _, ddl := range []string{
			fmt.Sprintf(`CREATE TABLE %s (org text, id int, owner text NOT NULL, v int NOT NULL DEFAULT 0, PRIMARY KEY (org, id))`, comp),
			fmt.Sprintf(`CREATE TABLE %s (id uuid PRIMARY KEY DEFAULT gen_random_uuid(), owner uuid NOT NULL, v int NOT NULL DEFAULT 0)`, uu),
		} {
			if _, err := e.pool.Exec(ctx, ddl); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { e.pool.Exec(context.Background(), "DROP TABLE "+comp+", "+uu) })
		if err := InstallTriggers(ctx, e.pool, comp, uu); err != nil {
			t.Fatal(err)
		}
		pair := func(name, table string, key []string, sql string, match func(json.RawMessage) (map[string]any, []any)) {
			for _, filtered := range []bool{true, false} {
				prefix := "u."
				if filtered {
					prefix = "f."
				}
				Publish(app, prefix+name, PubOptions{Tables: []string{table}, Key: key, Scope: ScopeShared},
					func(c *SubCtx, raw json.RawMessage) (Query, error) {
						m, args := match(raw)
						q := Query{SQL: sql, Args: args}
						if filtered {
							q.Match = m
						}
						return q, nil
					})
			}
		}
		owner := func(raw json.RawMessage) (map[string]any, []any) {
			var a struct{ Owner string }
			json.Unmarshal(raw, &a)
			return map[string]any{"owner": a.Owner}, []any{a.Owner}
		}
		pair("owner", e.table, nil, "SELECT id, owner, name, n FROM "+e.table+" WHERE owner = $1 ORDER BY id", owner)
		pair("n", e.table, nil, "SELECT id, owner, name, n FROM "+e.table+" WHERE n = $1 ORDER BY id DESC",
			func(raw json.RawMessage) (map[string]any, []any) {
				var a struct{ N int }
				json.Unmarshal(raw, &a)
				return map[string]any{"n": a.N}, []any{a.N}
			})
		pair("all", e.table, nil, "SELECT id, owner, name, n FROM "+e.table+" ORDER BY name, id",
			func(json.RawMessage) (map[string]any, []any) { return map[string]any{}, nil })
		pair("comp", comp, []string{"org", "id"}, "SELECT org, id, owner, v FROM "+comp+" WHERE owner = $1 ORDER BY org, id", owner)
		pair("uuid", uu, nil, "SELECT id, owner, v FROM "+uu+" WHERE owner = $1::uuid ORDER BY v, id", owner)
		Method(app, "exec", func(c *Call, a struct{ SQL []string }) (struct{}, error) {
			for _, s := range a.SQL {
				if _, err := c.Tx.Exec(c.Context, s); err != nil {
					return struct{}{}, err
				}
			}
			return struct{}{}, nil
		})
	})
}

// waitInits reads until n subs got their snapshot, failing on an end.
func (c *client) waitInits(n int) {
	c.t.Helper()
	for got := 0; got < n; {
		m := c.next()
		if m["t"] != "tx" {
			continue
		}
		for _, e := range entries(m) {
			if e["end"] != nil {
				c.t.Fatalf("sub %v ended: %v", e["s"], e["end"])
			}
			if e["ready"] == true {
				got++
			}
		}
	}
}

// execCall runs exec and returns each tx's entries by sub id, up to its res.
// Every tx is applied to rows, if not nil.
func (c *client) execCall(rows subRows, id int, sql ...string) []map[float64]string {
	c.t.Helper()
	c.send(map[string]any{"t": "call", "id": id, "m": "exec", "args": map[string]any{"SQL": sql}})
	var txs []map[float64]string
	for {
		m := c.next()
		switch m["t"] {
		case "tx":
			if rows != nil {
				rows.apply(c.t, m)
			}
			en := map[float64]string{}
			for _, e := range entries(m) {
				b, _ := json.Marshal(e["ops"])
				en[e["s"].(float64)] = string(b)
				if e["init"] != nil || e["end"] != nil {
					c.t.Fatalf("unexpected entry %v", e)
				}
			}
			txs = append(txs, en)
		case "res":
			if m["ok"] != true || m["lag"] == true {
				c.t.Fatalf("exec %v: %v", sql, m)
			}
			return txs
		}
	}
}

// The filtered path must send what a full re-run sends, for random writes.
// After every write (its res, so its fence) each f./u. pair of subs holds
// the same rows; when the write's effects arrive in one tx, as they do
// unless its notifications split across ticks, their ops are identical too.
func TestChangedKeyFilterMatchesFullRerun(t *testing.T) {
	fb := &fallbacks{}
	e := setupFilter(t, func(c *Config) { c.Logger = slog.New(fb) })
	comp, uu := e.table+"_c", e.table+"_u"
	owners := []string{"a", "b", "c"}
	uuids := []string{"00000000-0000-0000-0000-00000000000a", "00000000-0000-0000-0000-00000000000b", "00000000-0000-0000-0000-00000000000c"}
	a, _ := e.dial(t, nil)
	var subs [][2]any // name, args, for s = 2i+1 (f.) and 2i+2 (u.)
	for _, o := range owners {
		subs = append(subs, [2]any{"owner", map[string]any{"Owner": o}}, [2]any{"comp", map[string]any{"Owner": o}})
	}
	for _, u := range uuids {
		subs = append(subs, [2]any{"uuid", map[string]any{"Owner": u}})
	}
	for n := range 3 {
		subs = append(subs, [2]any{"n", map[string]any{"N": n}})
	}
	subs = append(subs, [2]any{"all", nil})
	for i, s := range subs {
		for j, prefix := range []string{"f.", "u."} {
			a.send(map[string]any{"t": "sub", "s": 2*i + j + 1, "name": prefix + s[0].(string), "args": s[1]})
		}
	}
	a.waitInits(2 * len(subs))
	rows := subRows{}

	rng := rand.New(rand.NewPCG(1, 2))
	pick := func(xs []string) string { return xs[rng.IntN(len(xs))] }
	op := func() []string {
		k := rng.IntN(5)
		switch rng.IntN(16) {
		case 0, 1, 2:
			return []string{fmt.Sprintf("INSERT INTO %s (owner, name, n) VALUES ('%s', 'r%d', %d)", e.table, pick(owners), rng.IntN(100), rng.IntN(3))}
		case 3:
			return []string{fmt.Sprintf("UPDATE %s SET owner = '%s' WHERE id %% 5 = %d", e.table, pick(owners), k)}
		case 4:
			return []string{fmt.Sprintf("UPDATE %s SET n = %d WHERE id %% 4 = %d", e.table, rng.IntN(3), k%4)}
		case 5:
			return []string{fmt.Sprintf("UPDATE %s SET name = name || 'x' WHERE id %% 3 = %d", e.table, k%3)}
		case 6:
			return []string{fmt.Sprintf("DELETE FROM %s WHERE id %% 7 = %d", e.table, k)}
		case 7:
			return []string{fmt.Sprintf("UPDATE %s SET id = id + 100000 WHERE id %% 6 = %d", e.table, k)}
		case 8, 9:
			return []string{fmt.Sprintf("INSERT INTO %s (org, id, owner, v) VALUES ('%s', %d, '%s', %d) ON CONFLICT DO NOTHING", comp, pick([]string{"x", "y<"}), rng.IntN(30), pick(owners), rng.IntN(9))}
		case 10:
			return []string{fmt.Sprintf("UPDATE %s SET owner = '%s', v = v + 1 WHERE id %% 4 = %d", comp, pick(owners), k%4)}
		case 11:
			// A primary-key change: the old key leaves, the new one arrives.
			return []string{fmt.Sprintf("UPDATE %s SET org = org || 'z' WHERE id %% 5 = %d AND length(org) < 4", comp, k)}
		case 12:
			return []string{fmt.Sprintf("DELETE FROM %s WHERE id %% 6 = %d", comp, k)}
		case 13:
			return []string{fmt.Sprintf("INSERT INTO %s (owner, v) VALUES ('%s', %d)", uu, pick(uuids), rng.IntN(9))}
		case 14:
			return []string{
				fmt.Sprintf("UPDATE %s SET owner = '%s' WHERE v %% 3 = %d", uu, pick(uuids), k%3),
				fmt.Sprintf("DELETE FROM %s WHERE v = %d", uu, rng.IntN(9)),
			}
		default:
			// Several statements, several tables, one transaction.
			return []string{
				fmt.Sprintf("INSERT INTO %s (owner, name, n) VALUES ('%s', 'm', %d)", e.table, pick(owners), rng.IntN(3)),
				fmt.Sprintf("UPDATE %s SET owner = '%s' WHERE id %% 9 = %d", e.table, pick(owners), k),
				fmt.Sprintf("UPDATE %s SET v = v + 1 WHERE owner = '%s'", comp, pick(owners)),
			}
		}
	}
	split := 0
	check := func(step int, sql []string, txs []map[float64]string) {
		t.Helper()
		for i := range subs {
			f, u := float64(2*i+1), float64(2*i+2)
			if !maps.EqualFunc(rows[f], rows[u], func(a, b []any) bool { return reflect.DeepEqual(a, b) }) {
				t.Fatalf("step %d %v: %s%v holds different rows:\nfiltered %v\nfull     %v", step, sql, subs[i][0], subs[i][1], rows[f], rows[u])
			}
			if len(txs) == 1 && txs[0][f] != txs[0][u] {
				t.Fatalf("step %d %v: %s%v ops differ:\nfiltered %s\nfull     %s", step, sql, subs[i][0], subs[i][1], txs[0][f], txs[0][u])
			}
		}
		if len(txs) > 1 {
			split++
		}
	}
	for step := range 300 {
		sql := op()
		check(step, sql, a.execCall(rows, step+1, sql...))
	}
	// Over 1,000 rows in one statement: a table-dirty marker, full re-runs.
	big := []string{fmt.Sprintf("INSERT INTO %s (owner, name, n) SELECT 'b', 'big', 1 FROM generate_series(1, 1100)", e.table)}
	check(300, big, a.execCall(rows, 301, big...))
	big = []string{fmt.Sprintf("DELETE FROM %s WHERE name = 'big'", e.table)}
	check(301, big, a.execCall(rows, 302, big...))
	t.Logf("%d of 302 writes arrived in more than one tx", split)
	if n := fb.n.Load(); n != 0 {
		t.Fatalf("filter fell back to full re-runs %d times", n)
	}
}

// countRuns subscribes n observers of pub, owner(i) for i < n, then
// reports how many observer queries the write runs.
func countRuns(t testing.TB, e *testEnv, pub string, n int, owner func(int) string, write string) int64 {
	t.Helper()
	e.waitListening(t)
	a, _ := e.dial(t, nil)
	for i := range n {
		a.send(map[string]any{"t": "sub", "s": i + 1, "name": pub, "args": map[string]any{"Owner": owner(i)}})
	}
	a.waitInits(n)
	before := e.app.engine.runs.Load()
	a.execCall(nil, 1, write)
	return e.app.engine.runs.Load() - before
}

// Acceptance: N observers on one table, one write: O(affected) queries with
// Match, O(N) without. Also for composite and uuid primary keys.
func TestChangedKeyFilterRunsOnlyAffected(t *testing.T) {
	const n = 200
	plain := func(i int) string { return fmt.Sprintf("o%d", i) }
	uuid := func(i int) string { return fmt.Sprintf("00000000-0000-0000-0000-%012d", i) }
	for _, c := range []struct {
		pub   string
		owner func(int) string
		write func(e *testEnv) string
	}{
		{"owner", plain, func(e *testEnv) string { return "INSERT INTO " + e.table + " (owner, name) VALUES ('o7', 'x')" }},
		{"comp", plain, func(e *testEnv) string { return "INSERT INTO " + e.table + "_c (org, id, owner) VALUES ('x', 1, 'o7')" }},
		{"uuid", uuid, func(e *testEnv) string { return "INSERT INTO " + e.table + "_u (owner) VALUES ('" + uuid(7) + "')" }},
	} {
		e := setupFilter(t, nil)
		if got := countRuns(t, e, "f."+c.pub, n, c.owner, c.write(e)); got != 1 {
			t.Errorf("f.%s: %d observer runs for one insert, want 1", c.pub, got)
		}
		e = setupFilter(t, nil)
		if got := countRuns(t, e, "u."+c.pub, n, c.owner, c.write(e)); got != n {
			t.Errorf("u.%s: %d observer runs, want %d", c.pub, got, n)
		}
	}
}

// BenchmarkChangedKeyFilter reports observer queries per write with N
// observers on one table, each write affecting one of them.
func BenchmarkChangedKeyFilter(b *testing.B) {
	for _, n := range []int{100, 1000} {
		for _, pub := range []string{"f.owner", "u.owner"} {
			b.Run(fmt.Sprintf("%s/N=%d", pub, n), func(b *testing.B) {
				e := setupFilter(b, func(c *Config) { c.MaxSubs = n })
				e.waitListening(b)
				a, _ := e.dial(b, nil)
				for i := range n {
					a.send(map[string]any{"t": "sub", "s": i + 1, "name": pub, "args": map[string]any{"Owner": fmt.Sprintf("o%d", i)}})
				}
				a.waitInits(n)
				before := e.app.engine.runs.Load()
				b.ResetTimer()
				for i := range b.N {
					a.execCall(nil, i+1, fmt.Sprintf("INSERT INTO %s (owner, name) VALUES ('o%d', 'x')", e.table, i%n))
				}
				b.ReportMetric(float64(e.app.engine.runs.Load()-before)/float64(b.N), "queries/write")
			})
		}
	}
}

// Columns sqlEqual cannot compare like `=`, and result keys that are not the
// table's primary key as-is, must fall back to full re-runs: char(n) pads,
// and a composite key with id::text builds different keys than the
// notification's (["a","1"] against ["a",1]).
func TestChangedKeyFilterFallsBackWhenItCannotCompare(t *testing.T) {
	var padded, comp string
	e := setupFull(t, nil, func(e *testEnv, app *App) {
		padded, comp = e.table+"_p", e.table+"_k"
		for _, ddl := range []string{
			"CREATE TABLE " + padded + " (id int PRIMARY KEY, owner char(8) NOT NULL)",
			"CREATE TABLE " + comp + " (org text, id int, owner text NOT NULL, PRIMARY KEY (org, id))",
		} {
			if _, err := e.pool.Exec(context.Background(), ddl); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { e.pool.Exec(context.Background(), "DROP TABLE "+padded+", "+comp) })
		if err := InstallTriggers(context.Background(), e.pool, padded, comp); err != nil {
			t.Fatal(err)
		}
		Publish(app, "padded", PubOptions{Tables: []string{padded}, Scope: ScopeShared},
			func(c *SubCtx, _ struct{}) (Query, error) {
				return Query{SQL: "SELECT id, owner FROM " + padded + " WHERE owner = 'bob' ORDER BY id", Match: map[string]any{"owner": "bob"}}, nil
			})
		Publish(app, "castKey", PubOptions{Tables: []string{comp}, Key: []string{"org", "id"}, Scope: ScopeShared},
			func(c *SubCtx, _ struct{}) (Query, error) {
				return Query{SQL: "SELECT org, id::text AS id FROM " + comp + " WHERE owner = 'bob' ORDER BY org, id", Match: map[string]any{"owner": "bob"}}, nil
			})
		Method(app, "exec", func(c *Call, a struct{ SQL []string }) (struct{}, error) {
			for _, s := range a.SQL {
				if _, err := c.Tx.Exec(c.Context, s); err != nil {
					return struct{}{}, err
				}
			}
			return struct{}{}, nil
		})
	})
	a, _ := e.dial(t, nil)
	a.send(map[string]any{"t": "sub", "s": 1, "name": "padded"})
	a.send(map[string]any{"t": "sub", "s": 2, "name": "castKey"})
	a.waitInits(2)
	rows := subRows{}
	a.execCall(rows, 1, "INSERT INTO "+padded+" VALUES (1, 'bob')", "INSERT INTO "+comp+" VALUES ('a', 1, 'bob')")
	if len(rows[1]) != 1 || len(rows[2]) != 1 {
		t.Fatalf("inserts missed: %v", rows)
	}
	// The row stops matching: only the result's key can tell.
	a.execCall(rows, 2, "UPDATE "+padded+" SET owner = 'al'", "UPDATE "+comp+" SET owner = 'al'")
	if len(rows[1]) != 0 || len(rows[2]) != 0 {
		t.Fatalf("rows that stopped matching kept: %v", rows)
	}
}

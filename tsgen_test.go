package cometgo

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"
)

type tsInner struct {
	A int `json:"a"`
}

type tsEmbedded struct {
	E string
	X int `json:"x"` // shadowed by the outer x
}

type tsRecursive struct {
	Next *tsRecursive `json:"next"`
}

type tsAll struct {
	Str      string            `json:"str"`
	Skip     string            `json:"-"`
	Dash     string            `json:"-,"`
	Opt      int               `json:"opt,omitempty"`
	Zero     int               `json:"zero,omitzero"`
	BigID    int64             `json:"bigId,string"`
	PtrID    *int64            `json:"ptrId,string"`
	Flag     bool              `json:",string"`
	When     time.Time         `json:"when"`
	Bytes    []byte            `json:"bytes"`
	Fixed    [2]byte           `json:"fixed"`
	List     []string          `json:"list"`
	Ptrs     []*tsInner        `json:"ptrs"`
	M        map[string]int    `json:"m"`
	Raw      json.RawMessage   `json:"raw"`
	Num      json.Number       `json:"num"`
	Any      any               `json:"any"`
	Addr     netip.Addr        `json:"addr"` // TextMarshaler
	Big      *big.Int          `json:"big"`  // json.Marshaler
	Inner    tsInner           `json:"inner"`
	InnerPtr *tsInner          `json:"innerPtr"`
	Rec      tsRecursive       `json:"rec"`
	Nested   map[string][]bool `json:"nested"`
	X        string            `json:"x"`
	tsEmbedded
	unexported int
	NoTag      float64
}

func TestJSONTS(t *testing.T) {
	for _, c := range []struct {
		v    any
		want string
	}{
		{struct{}{}, "Record<string, never>"},
		{int64(0), "number"},
		{"", "string"},
		{[]int{}, "number[] | null"},
		{[]*int{}, "(number | null)[] | null"},
		{map[int]string{}, "Record<string, string> | null"},
		{(*string)(nil), "string | null"},
		{struct {
			B   int `json:"b"`
			bad int
		}{}, "{ b: number }"},
	} {
		if got := jsonTS(reflect.TypeOf(c.v)); got != c.want {
			t.Errorf("%T: got %s, want %s", c.v, got, c.want)
		}
	}
}

func TestJSONTSStructRules(t *testing.T) {
	got := jsonTS(reflect.TypeFor[tsAll]())
	want := "{ str: string; \"-\": string; opt?: number; zero?: number; bigId: string; ptrId: string | null; Flag: string; " +
		"when: string; bytes: string | null; fixed: number[]; list: string[] | null; ptrs: ({ a: number } | null)[] | null; " +
		"m: Record<string, number> | null; raw: unknown; num: number; any: unknown; addr: string; big: unknown; " +
		"inner: { a: number }; innerPtr: { a: number } | null; rec: { next: unknown }; " +
		"nested: Record<string, boolean[] | null> | null; x: string; E: string; NoTag: number }"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

// The field names jsonTS lists are exactly the keys encoding/json writes.
func TestJSONTSKeysMatchEncodingJSON(t *testing.T) {
	b, err := json.Marshal(tsAll{Opt: 1, Zero: 1})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	fields := jsonFields(reflect.TypeFor[tsAll]())
	if len(fields) != len(m) {
		t.Fatalf("%d fields, encoding/json wrote %d keys: %s", len(fields), len(m), b)
	}
	for _, f := range fields {
		if _, ok := m[f.name]; !ok {
			t.Errorf("field %s not in %s", f.name, b)
		}
	}
}

type tsD struct{ X int }
type tsB struct{ tsD }
type tsC struct{ tsD }

// tsTwice embeds tsD twice at one depth: encoding/json drops X.
type tsTwice struct {
	tsB
	tsC
	Y int
}

type tsTree map[string]tsTree
type tsList []tsList

// tsPtrText has MarshalText only on its pointer.
type tsPtrText struct{ V int }

func (p *tsPtrText) MarshalText() ([]byte, error) { return []byte("t"), nil }

// tsUnText decodes from a string through UnmarshalText on its pointer.
type tsUnText struct{ V int }

func (p *tsUnText) UnmarshalText([]byte) error { return nil }

func TestJSONTSDuplicateEmbeddingAnnihilates(t *testing.T) {
	b, _ := json.Marshal(tsTwice{})
	if string(b) != `{"Y":0}` {
		t.Fatalf("encoding/json wrote %s", b)
	}
	if got := jsonTS(reflect.TypeFor[tsTwice]()); got != "{ Y: number }" {
		t.Fatalf("got %s", got)
	}
}

func TestJSONTSRecursiveNamedTypes(t *testing.T) {
	if got := jsonTS(reflect.TypeFor[tsTree]()); got != "Record<string, unknown> | null" {
		t.Errorf("tree: %s", got)
	}
	if got := jsonTS(reflect.TypeFor[tsList]()); got != "unknown[] | null" {
		t.Errorf("list: %s", got)
	}
}

// Pointer-receiver marshalers count only where encoding/json can take the
// address, which the type checks against what json.Marshal writes.
func TestJSONTSPointerReceiverMarshaler(t *testing.T) {
	type res struct {
		Val   tsPtrText            `json:"val"`
		Ptr   *tsPtrText           `json:"ptr"`
		Slice []tsPtrText          `json:"slice"`
		Map   map[string]tsPtrText `json:"map"`
	}
	v := res{Ptr: &tsPtrText{}, Slice: []tsPtrText{{}}, Map: map[string]tsPtrText{"k": {}}}
	direct, _ := json.Marshal(v)  // not addressable: Val and Map hold objects
	viaPtr, _ := json.Marshal(&v) // addressable: Val is a string
	if string(direct) != `{"val":{"V":0},"ptr":"t","slice":["t"],"map":{"k":{"V":0}}}` || !strings.Contains(string(viaPtr), `"val":"t"`) {
		t.Fatalf("encoding/json wrote %s and %s", direct, viaPtr)
	}
	if got, want := jsonTS(reflect.TypeFor[res]()), "{ val: { V: number }; ptr: string | null; slice: string[] | null; map: Record<string, { V: number }> | null }"; got != want {
		t.Errorf("by value\ngot  %s\nwant %s", got, want)
	}
	if got := jsonTS(reflect.TypeFor[*res]()); !strings.HasPrefix(got, "{ val: string;") {
		t.Errorf("by pointer: %s", got)
	}
	// Args are decoded into an addressable value: UnmarshalText counts.
	type args struct {
		U tsUnText `json:"u"`
	}
	if got := argsTS(reflect.TypeFor[args]()); got != "{ u: string }" {
		t.Errorf("args: %s", got)
	}
	if err := json.Unmarshal([]byte(`{"u":"x"}`), &args{}); err != nil {
		t.Errorf("encoding/json rejects a string for u: %v", err)
	}
}

func TestRowCols(t *testing.T) {
	type row struct {
		ID      int64      `json:"id"`
		Title   string     `json:"title"`
		Done    *bool      `json:"done"`
		At      time.Time  `json:"at"`
		Tags    []string   `json:"tags"`
		Data    []byte     `json:"data"`
		Meta    any        `json:"meta"`
		Small   int16      `json:"small"`
		Score   *float64   `json:"score"`
		Removed *time.Time `json:"removed"`
	}
	cols, err := rowCols(reflect.TypeFor[row]())
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(cols)
	want := "[{id i8 false []} {title text false []} {done bool true []} {at tstz false []} {tags []text false []} " +
		"{data bytes false []} {meta json false []} {small i4 false []} {score f8 true []} {removed tstz true []}]"
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if _, err := rowCols(reflect.TypeFor[int]()); err == nil {
		t.Fatal("non-struct Row accepted")
	}
	type tagged struct {
		ID   netip.Addr `json:"id"`                 // TextMarshaler: a string
		Day  time.Time  `json:"day" cometgo:"date"` // a date column
		Tags []string   `json:"tags" cometgo:"[]enum"`
	}
	cols, err = rowCols(reflect.TypeFor[tagged]())
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(cols); got != "[{id text false []} {day date false []} {tags []enum false []}]" {
		t.Fatalf("tagged: %s", got)
	}
	if _, err := rowCols(reflect.TypeFor[struct {
		ID [16]byte `json:"id"`
	}]()); err == nil || !strings.Contains(err.Error(), "cometgo tag") {
		t.Fatalf("[16]byte without a tag: %v", err)
	}
	if _, err := rowCols(reflect.TypeFor[struct {
		ID string `json:"id" cometgo:"varchar"`
	}]()); err == nil {
		t.Fatal("unknown cometgo code accepted")
	}
}

func TestCodeTS(t *testing.T) {
	for code, want := range map[string]string{
		"i4": "number", "i8": "string", "f8": "number", "num": "string", "bool": "boolean", "text": "string",
		"uuid": "string", "tstz": "Date", "ts": "string", "date": "string", "json": "unknown",
		"bytes": "Uint8Array", "raw": "string", "enum": "string", "[]i4": "(number | null)[]",
		"[]tstz": "(Date | null)[]",
	} {
		if got := codeTS(code, nil); got != want {
			t.Errorf("%s: got %s, want %s", code, got, want)
		}
	}
	if got := codeTS("[]enum", []string{"a", `b"c`}); got != `("a" | "b\"c" | null)[]` {
		t.Errorf("[]enum: %s", got)
	}
}

func TestDescribe(t *testing.T) {
	e := setupFull(t, nil, func(e *testEnv, app *App) {
		ctx := context.Background()
		for _, q := range []string{
			`DO $$ BEGIN CREATE TYPE ts_mood AS ENUM ('sad', 'ok', 'happy'); EXCEPTION WHEN duplicate_object THEN NULL; END $$`,
			fmt.Sprintf(`CREATE TABLE %s_d (id bigserial PRIMARY KEY, title text NOT NULL, note text, mood ts_mood NOT NULL,
				moods ts_mood[], at timestamptz NOT NULL DEFAULT now(), data bytea, meta jsonb, n int4 NOT NULL DEFAULT 0, amount numeric)`, e.table),
		} {
			if _, err := e.pool.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { e.pool.Exec(context.Background(), "DROP TABLE "+e.table+"_d") })
		if err := installFeed(ctx, e.pool, e.table+"_d"); err != nil {
			t.Fatal(err)
		}
		Publish(app, "d.one", PubOptions{Tables: []string{e.table + "_d"}},
			func(c *SubCtx, a struct {
				Mood string `json:"mood"`
			}) (Query, error) {
				return Query{SQL: "SELECT id, title, note, mood, moods, at, data, meta, n + 1 AS n1, amount FROM " + e.table + "_d WHERE mood::text <> $1 ORDER BY id;", Args: []any{a.Mood}}, nil
			})
		Publish(app, "d.two", PubOptions{Tables: []string{e.table + "_d", e.table}},
			func(c *SubCtx, _ struct{}) (Query, error) {
				return Query{SQL: "SELECT d.id, d.title, i.name FROM " + e.table + "_d d LEFT JOIN " + e.table + " i ON i.id = d.id"}, nil
			})
		Publish(app, "d.unnest", PubOptions{Tables: []string{e.table + "_d"}},
			func(c *SubCtx, _ struct{}) (Query, error) {
				return Query{SQL: "SELECT u.id, d.title FROM unnest('{1,2}'::bigint[]) u(id) LEFT JOIN " + e.table + "_d d ON d.id = u.id"}, nil
			})
		Publish(app, "d.rollup", PubOptions{Tables: []string{e.table + "_d"}, Key: []string{"k"}},
			func(c *SubCtx, _ struct{}) (Query, error) {
				return Query{SQL: "SELECT row_number() OVER () AS k, mood FROM " + e.table + "_d GROUP BY ROLLUP (mood)"}, nil
			})
		Publish(app, "d.dup", PubOptions{Tables: []string{e.table + "_d"}},
			func(c *SubCtx, _ struct{}) (Query, error) {
				return Query{SQL: "SELECT id, title, n AS title FROM " + e.table + "_d"}, nil
			})
		type row struct {
			ID   string  `json:"id"`
			Name *string `json:"name"`
		}
		Publish(app, "d.row", PubOptions{Tables: []string{e.table}, Row: row{}},
			func(c *SubCtx, _ struct{}) (Query, error) { return Query{}, fmt.Errorf("must not be called") })
	})
	ctx := context.Background()
	if _, err := Describe(ctx, e.app, DescribeOptions{}); err == nil || !strings.Contains(err.Error(), "items.mine") {
		t.Fatalf("anonymous describe of items.mine: %v", err)
	}
	panicky := setupFull(t, nil, func(pe *testEnv, app *App) {
		Publish(app, "p", PubOptions{Tables: []string{pe.table}}, func(c *SubCtx, _ struct{}) (Query, error) {
			return Query{SQL: "SELECT " + c.Identity.UserID}, nil // nil Identity
		})
	})
	if _, err := newEngine(panicky.app).describeQuery(ctx, panicky.app.pubs["p"], nil); err == nil || !strings.Contains(err.Error(), "panic") {
		t.Fatalf("builder panic not reported as an error: %v", err)
	}
	s, err := Describe(ctx, e.app, DescribeOptions{Identity: &Identity{UserID: "u"}})
	if err != nil {
		t.Fatal(err)
	}
	pubs := map[string]PubSchema{}
	for _, p := range s.Publications {
		pubs[p.Name] = p
	}
	ts := func(p string) string { pp := pubs[p]; return pp.rowTS() }
	if got, want := ts("d.one"), `{ id: string; title: string; note: string | null; mood: "sad" | "ok" | "happy"; `+
		`moods: ("sad" | "ok" | "happy" | null)[] | null; at: Date; data: Uint8Array | null; meta: unknown; `+
		`n1: number | null; amount: string | null }`; got != want {
		t.Errorf("d.one row\ngot  %s\nwant %s", got, want)
	}
	if got := pubs["d.one"].Args; got != "{ mood: string }" {
		t.Errorf("d.one args %s", got)
	}
	// Outer join against a function, and grouping sets, can put null in
	// NOT NULL columns of the one table read. The key never is null.
	if got, want := ts("d.unnest"), "{ id: string; title: string | null }"; got != want {
		t.Errorf("d.unnest row %s, want %s", got, want)
	}
	if got, want := ts("d.rollup"), `{ k: string; mood: "sad" | "ok" | "happy" | null }`; got != want {
		t.Errorf("d.rollup row %s, want %s", got, want)
	}
	if got, want := ts("d.dup"), "{ id: string; title: number }"; got != want {
		t.Errorf("d.dup row %s, want %s", got, want)
	}
	if got, want := ts("d.two"), "{ id: string; title: string | null; name: string | null }"; got != want {
		t.Errorf("d.two row %s, want %s", got, want)
	}
	if got, want := ts("d.row"), "{ id: string; name: string | null }"; got != want {
		t.Errorf("d.row row %s, want %s", got, want)
	}
	if got := pubs["items.all"].Args; got != "Record<string, never> | null" {
		t.Errorf("items.all args %s", got)
	}

	out := string(s.TypeScript())
	for _, want := range []string{
		"export interface Methods {\n  add: {\n    args: { Owner: string; Name: string };\n    result: string;\n  };\n",
		"  bump: {\n    args: { ID: string };\n    result: Record<string, never>;\n  };\n",
		`  "d.one": {`,
		"export interface Schema {\n  methods: Methods;\n  publications: Publications;\n}\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// Sorted, so stable.
	if strings.Index(out, "  add:") > strings.Index(out, "  bump:") || strings.Index(out, `"d.one"`) > strings.Index(out, `"items.all"`) {
		t.Errorf("not sorted:\n%s", out)
	}
	s2, err := Describe(ctx, e.app, DescribeOptions{Identity: &Identity{UserID: "u"}})
	if err != nil || string(s2.TypeScript()) != out {
		t.Fatalf("second describe differs (err %v)", err)
	}
}

package cometgo

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Changed-key filtering (PROTOCOL.md §11): an observer whose query declares
// Query.Match is re-run only when a changed row matches its predicate, or
// when its result holds one of the changed keys (a delete, or a row that
// stopped matching). Everything it cannot decide with certainty falls back
// to a full re-run.

// tableChange is what one tick learned about one table: the changed primary
// keys, or all when a table-dirty marker (or anything unparseable) arrived.
type tableChange struct {
	all  bool
	keys []json.RawMessage // each a JSON array of PK values, wire-encoded
}

func (tc *tableChange) add(n notification) {
	if tc.all {
		return
	}
	var keys []json.RawMessage
	if n.keys == "" || json.Unmarshal([]byte(n.keys), &keys) != nil {
		tc.all, tc.keys = true, nil
		return
	}
	tc.keys = append(tc.keys, keys...)
}

// pkInfo is a table's primary key, in index order (the order of the PK
// arrays in its notifications).
type pkInfo struct {
	cols  []string
	types []string // format_type, e.g. "bigint", "uuid"
}

// changedRows is a table's changed rows as of the tick snapshot. Deleted
// rows are absent; canon holds every changed key in rowKey form.
type changedRows struct {
	pk    *pkInfo
	canon []string
	rows  []map[string]any
}

// filterable reports whether obs can skip a re-run for changes to its one
// table, before the snapshot: the rest is checked in affected.
func (obs *observer) filterable(tc *tableChange) bool {
	return obs.q.Match != nil && len(obs.tables) == 1 && tc != nil && !tc.all && obs.res != nil
}

// affected reports whether changes can alter obs's result. Anything it
// cannot decide counts as affected.
func (obs *observer) affected(ch *changedRows) bool {
	if ch == nil || !slices.Equal(obs.pub.opts.Key, ch.pk.cols) || len(obs.keyIdx) != len(ch.pk.cols) {
		return true
	}
	for _, i := range obs.keyIdx {
		switch obs.cols[i].code {
		case "i4", "i8", "text", "uuid":
		default:
			return true // a key whose wire form may differ from the notification's
		}
	}
	for _, k := range ch.canon {
		if _, ok := obs.res.rows[k]; ok {
			return true
		}
	}
	for _, row := range ch.rows {
		if m, ok := matches(row, obs.q.Match); !ok || m {
			return true
		}
	}
	return false
}

// matches evaluates the conjunction of col = value over row with SQL
// semantics. ok is false when it cannot tell (unknown column, a type it
// does not compare).
func matches(row map[string]any, match map[string]any) (m, ok bool) {
	m = true
	for col, want := range match {
		got, present := row[col]
		if !present {
			return false, false
		}
		eq, ok := sqlEqual(got, want)
		if !ok {
			return false, false
		}
		m = m && eq
	}
	return m, true
}

// sqlEqual compares a value pgx decoded with a Match value as `=` would.
// NULL equals nothing. ok is false for types it does not compare.
func sqlEqual(got, want any) (eq, ok bool) {
	if want == nil {
		return false, false // = NULL is never true, but the caller probably meant IS NULL
	}
	if got == nil {
		return false, true
	}
	if gi, ok := asInt(got); ok {
		if wi, ok := asInt(want); ok {
			return gi == wi, true
		}
		return false, false
	}
	switch g := got.(type) {
	case string:
		if w, ok := want.(string); ok {
			return g == w, true
		}
	case bool:
		if w, ok := want.(bool); ok {
			return g == w, true
		}
	case [16]byte:
		switch w := want.(type) {
		case [16]byte:
			return g == w, true
		case string:
			if u, ok := parseUUID(w); ok {
				return g == u, true
			}
		}
	}
	return false, false
}

func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}

func parseUUID(s string) ([16]byte, bool) {
	var u [16]byte
	h := strings.ReplaceAll(s, "-", "")
	if len(h) != 32 {
		return u, false
	}
	for i := range 16 {
		b, err := strconv.ParseUint(h[2*i:2*i+2], 16, 8)
		if err != nil {
			return u, false
		}
		u[i] = byte(b)
	}
	return u, true
}

// canonKeys converts notification PK arrays to rowKey form, and to the text
// of each value for the fetch query. ok is false for a value that is not a
// string or a number.
func canonKeys(keys []json.RawMessage, ncols int) (canon []string, texts [][]string, ok bool) {
	texts = make([][]string, ncols)
	for _, raw := range keys {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		var vals []any
		if dec.Decode(&vals) != nil || len(vals) != ncols {
			return nil, nil, false
		}
		var sb strings.Builder
		if ncols > 1 {
			sb.WriteByte('[')
		}
		for i, v := range vals {
			var text string
			var cell json.RawMessage
			switch v := v.(type) {
			case string:
				text, cell = v, quote(v)
			case json.Number:
				text, cell = v.String(), json.RawMessage(v.String())
			default:
				return nil, nil, false
			}
			texts[i] = append(texts[i], text)
			switch {
			case ncols == 1:
				sb.WriteString(text) // rowKey: a single key's wire string, unquoted
			default:
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.Write(cell)
			}
		}
		if ncols > 1 {
			sb.WriteByte(']')
		}
		canon = append(canon, sb.String())
	}
	return canon, texts, true
}

// loadPK reads table's primary key, cached for the engine's lifetime. A
// schema change that alters a primary key needs a restart, as it does for
// the triggers (InstallTriggers).
func (e *engine) loadPK(ctx context.Context, tx pgx.Tx, table string) (*pkInfo, error) {
	if pk := e.pks[table]; pk != nil {
		return pk, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT a.attname, format_type(a.atttypid, a.atttypmod)
		FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
		WHERE i.indrelid = to_regclass($1) AND i.indisprimary
		ORDER BY array_position(i.indkey::int2[], a.attnum)`, table)
	if err != nil {
		return nil, err
	}
	pk := &pkInfo{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			rows.Close()
			return nil, err
		}
		pk.cols = append(pk.cols, name)
		pk.types = append(pk.types, typ)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(pk.cols) == 0 {
		return nil, fmt.Errorf("%s has no primary key", table)
	}
	e.pks[table] = pk
	return pk, nil
}

// lateTables reports the tables with notifications not yet drained, or all
// for a notification that dirties everything.
func (e *engine) lateTables() (all bool, tables map[string]bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tables = map[string]bool{}
	for _, n := range e.pending {
		switch {
		case n.all:
			all = true
		case n.table != "":
			tables[n.table] = true
		}
	}
	return all, tables
}

// fetchChanged reads tc's changed rows inside the tick snapshot.
func (e *engine) fetchChanged(ctx context.Context, tx pgx.Tx, table string, tc *tableChange) (*changedRows, error) {
	pk, err := e.loadPK(ctx, tx, table)
	if err != nil {
		return nil, err
	}
	canon, texts, ok := canonKeys(tc.keys, len(pk.cols))
	if !ok {
		return nil, fmt.Errorf("unrecognised key in notification for %s", table)
	}
	schema, rel, _ := strings.Cut(table, ".")
	qtable := pgx.Identifier{schema, rel}.Sanitize()
	cols := make([]string, len(pk.cols))
	params := make([]string, len(pk.cols))
	args := make([]any, len(pk.cols))
	for i, c := range pk.cols {
		cols[i] = pgx.Identifier{c}.Sanitize()
		params[i] = fmt.Sprintf("$%d::text[]::%s[]", i+1, pk.types[i])
		args[i] = texts[i]
	}
	var sql string
	if len(cols) == 1 {
		sql = fmt.Sprintf("SELECT * FROM %s WHERE %s = ANY(%s)", qtable, cols[0], params[0])
	} else {
		sql = fmt.Sprintf("SELECT * FROM %s WHERE (%s) IN (SELECT * FROM unnest(%s))", qtable, strings.Join(cols, ", "), strings.Join(params, ", "))
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fds := rows.FieldDescriptions()
	ch := &changedRows{pk: pk, canon: canon}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		row := make(map[string]any, len(fds))
		for i, fd := range fds {
			row[fd.Name] = vals[i]
		}
		ch.rows = append(ch.rows, row)
	}
	return ch, rows.Err()
}

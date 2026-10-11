package cometgo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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

// tableInfo is what filtering needs to know about a table, read from the
// catalog once per engine. A schema change to the table's primary key or to
// a Match column's type needs a restart, as the triggers do.
type tableInfo struct {
	qname  string   // quoted schema.table
	pk     []string // primary key, in index order (the notifications' order)
	pkOIDs []uint32
	pkType []string // format_type, e.g. "bigint"
	// comparable holds the columns whose values sqlEqual compares as `=`
	// does: integers, text/varchar with a deterministic collation, bool,
	// uuid. Others (char(n) and its padding, citext, floats, …) are absent.
	comparable map[string]bool
	err        error // the table cannot be filtered
}

// filterCandidates returns the candidates that changes can affect, inside
// the snapshot of tx. Failures only cost the filtering: a candidate that
// cannot be decided is affected.
func (e *engine) filterCandidates(ctx context.Context, tx pgx.Tx, cands []*observer, changes map[string]*tableChange) ([]*observer, error) {
	// Take the snapshot before looking at notifications that arrived after
	// the drain: theirs may be commits it already holds, so their keys are
	// checked too. Notifications still in flight from Postgres slip through
	// and are caught next tick, as for observers of other tables.
	if _, err := tx.Exec(ctx, "SELECT 1"); err != nil {
		return nil, err
	}
	lateAll, late := e.lateChanges()

	byTable := map[string][]*observer{}
	for _, obs := range cands {
		byTable[obs.tables[0]] = append(byTable[obs.tables[0]], obs)
	}
	var affected []*observer
	for t, obsList := range byTable {
		tc := &tableChange{all: changes[t].all, keys: slices.Clone(changes[t].keys)}
		if lt := late[t]; lt != nil {
			tc.all = tc.all || lt.all
			tc.keys = append(tc.keys, lt.keys...)
		}
		if lateAll || tc.all {
			affected = append(affected, obsList...)
			continue
		}
		var info *tableInfo
		err := savepoint(ctx, tx, func(sp pgx.Tx) (err error) {
			info, err = e.loadTable(ctx, sp, t)
			return err
		})
		if err == nil && info.err != nil {
			err = info.err
		}
		if err != nil {
			e.a.log.Debug("changed-key filter unavailable, re-running", "table", t, "err", err)
			affected = append(affected, obsList...)
			continue
		}
		var eligible []*observer
		cols := slices.Clone(info.pk)
		for _, obs := range obsList {
			if !obs.eligible(info) {
				affected = append(affected, obs)
				continue
			}
			eligible = append(eligible, obs)
			for c := range obs.q.Match {
				if !slices.Contains(cols, c) {
					cols = append(cols, c)
				}
			}
		}
		if len(eligible) == 0 {
			continue
		}
		var ch *changedRows
		err = savepoint(ctx, tx, func(sp pgx.Tx) (err error) {
			ch, err = fetchChanged(ctx, sp, info, tc.keys, cols)
			return err
		})
		if err != nil {
			e.a.log.Debug("changed-key filter unavailable, re-running", "table", t, "err", err)
			affected = append(affected, eligible...)
			continue
		}
		for _, obs := range eligible {
			if obs.affected(ch) {
				affected = append(affected, obs)
			}
		}
	}
	return affected, nil
}

// savepoint runs fn in a savepoint of tx, so its failure leaves tx usable.
func savepoint(ctx context.Context, tx pgx.Tx, fn func(pgx.Tx) error) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	if err := fn(sp); err != nil {
		if rerr := sp.Rollback(ctx); rerr != nil {
			return errors.Join(err, rerr)
		}
		return err
	}
	return sp.Commit(ctx)
}

// lateChanges collects the notifications not yet drained, by table, and
// whether one dirties everything.
func (e *engine) lateChanges() (all bool, tables map[string]*tableChange) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tables = map[string]*tableChange{}
	for _, n := range e.pending {
		switch {
		case n.all:
			all = true
		case n.table != "":
			tc := tables[n.table]
			if tc == nil {
				tc = &tableChange{}
				tables[n.table] = tc
			}
			tc.add(n)
		}
	}
	return all, tables
}

// filterable reports whether obs can skip a re-run for changes to its one
// table, before the snapshot: the rest is decided in filterCandidates.
func (obs *observer) filterable(tc *tableChange) bool {
	return obs.q.Match != nil && len(obs.tables) == 1 && tc != nil && !tc.all && obs.res != nil
}

// eligible reports whether obs's result can be checked against changed
// keys and rows of info's table: its key is the table's primary key, with
// the same types (so the notification's keys encode as its row keys), and
// every Match column is one sqlEqual compares.
func (obs *observer) eligible(info *tableInfo) bool {
	if !slices.Equal(obs.pub.opts.Key, info.pk) || len(obs.keyIdx) != len(info.pk) {
		return false
	}
	for j, i := range obs.keyIdx {
		c := obs.cols[i]
		if c.oid != info.pkOIDs[j] {
			return false // e.g. id::text AS id
		}
		switch c.code {
		case "i4", "i8", "text", "uuid":
		default:
			return false
		}
	}
	for c := range obs.q.Match {
		if !info.comparable[c] {
			return false
		}
	}
	return true
}

// changedRows is a table's changed rows as of the tick snapshot. Deleted
// rows are absent; canon holds every changed key in rowKey form.
type changedRows struct {
	canon []string
	rows  []map[string]any
}

// affected reports whether ch can alter obs's result. obs is eligible.
func (obs *observer) affected(ch *changedRows) bool {
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
// semantics. ok is false when it cannot tell.
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

// sqlEqual compares a value pgx decoded from a comparable column with a
// Match value as `=` would. NULL equals nothing. ok is false for value
// types it does not compare.
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
			var u pgtype.UUID
			if u.Scan(w) == nil {
				return g == u.Bytes, true
			}
		}
	}
	return false, false
}

func asInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int8:
		return int64(n), true
	case int16:
		return int64(n), true
	case int32:
		return int64(n), true
	case int64:
		return n, true
	case uint8:
		return int64(n), true
	case uint16:
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint:
		if uint64(n) <= math.MaxInt64 {
			return int64(n), true
		}
	case uint64:
		if n <= math.MaxInt64 {
			return int64(n), true
		}
	}
	return 0, false
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

// comparableOIDs are the column types sqlEqual compares as `=` does.
var comparableOIDs = map[uint32]bool{
	pgtype.Int2OID: true, pgtype.Int4OID: true, pgtype.Int8OID: true,
	pgtype.TextOID: true, pgtype.VarcharOID: true,
	pgtype.BoolOID: true, pgtype.UUIDOID: true,
}

// loadTable reads table's metadata, cached for the engine's lifetime. A
// table that cannot be filtered is cached too, with err set; a failed
// query is not.
func (e *engine) loadTable(ctx context.Context, tx pgx.Tx, table string) (*tableInfo, error) {
	if info := e.tables[table]; info != nil {
		return info, nil
	}
	schema, rel, _ := strings.Cut(table, ".")
	info := &tableInfo{qname: pgx.Identifier{schema, rel}.Sanitize(), comparable: map[string]bool{}}
	rows, err := tx.Query(ctx, `
		SELECT a.attname, a.atttypid, format_type(a.atttypid, a.atttypmod),
		       -- indkey::int2[] is 0-based: test membership, order by position.
		       CASE WHEN a.attnum = ANY(i.indkey) THEN array_position(i.indkey::int2[], a.attnum) END,
		       COALESCE(c.collisdeterministic, true)
		FROM pg_attribute a
		LEFT JOIN pg_index i ON i.indrelid = a.attrelid AND i.indisprimary
		LEFT JOIN pg_collation c ON c.oid = a.attcollation
		WHERE a.attrelid = to_regclass($1) AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY 4 NULLS FIRST`, info.qname)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name, typ string
		var oid uint32
		var pkPos *int32
		var deterministic bool
		if err := rows.Scan(&name, &oid, &typ, &pkPos, &deterministic); err != nil {
			rows.Close()
			return nil, err
		}
		if pkPos != nil {
			info.pk = append(info.pk, name)
			info.pkOIDs = append(info.pkOIDs, oid)
			info.pkType = append(info.pkType, typ)
		}
		if comparableOIDs[oid] && deterministic {
			info.comparable[name] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(info.pk) == 0 {
		info.err = fmt.Errorf("%s not found or has no primary key", table)
	}
	e.tables[table] = info
	return info, nil
}

// fetchChanged reads the changed rows' cols inside the tick snapshot. cols
// starts with the primary key.
func fetchChanged(ctx context.Context, tx pgx.Tx, info *tableInfo, keys []json.RawMessage, cols []string) (*changedRows, error) {
	canon, texts, ok := canonKeys(keys, len(info.pk))
	if !ok {
		return nil, fmt.Errorf("unrecognised key in notification for %s", info.qname)
	}
	sel := make([]string, len(cols))
	for i, c := range cols {
		sel[i] = pgx.Identifier{c}.Sanitize()
	}
	pkCols := sel[:len(info.pk)]
	params := make([]string, len(info.pk))
	args := make([]any, len(info.pk))
	for i := range info.pk {
		params[i] = fmt.Sprintf("$%d::text[]::%s[]", i+1, info.pkType[i])
		args[i] = texts[i]
	}
	var sql string
	if len(pkCols) == 1 {
		sql = fmt.Sprintf("SELECT %s FROM %s WHERE %s = ANY(%s)", strings.Join(sel, ", "), info.qname, pkCols[0], params[0])
	} else {
		sql = fmt.Sprintf("SELECT %s FROM %s WHERE (%s) IN (SELECT * FROM unnest(%s))", strings.Join(sel, ", "), info.qname, strings.Join(pkCols, ", "), strings.Join(params, ", "))
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ch := &changedRows{canon: canon}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = vals[i]
		}
		ch.rows = append(ch.rows, row)
	}
	return ch, rows.Err()
}

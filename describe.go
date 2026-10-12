package cometgo

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Schema describes an App's methods and publications, for generating
// client types (Schema.TypeScript).
type Schema struct {
	Methods      []MethodSchema // sorted by name
	Publications []PubSchema    // sorted by name
}

// MethodSchema describes a method. Args and Result are TypeScript types of
// the JSON the method accepts and returns.
type MethodSchema struct {
	Name, Args, Result string
}

// PubSchema describes a publication: the TypeScript type of its args, and
// the columns of its rows.
type PubSchema struct {
	Name, Args string
	Cols       []ColSchema
}

// ColSchema is one result column: its wire type code (PROTOCOL.md §10),
// whether it can be null, and for an enum (or array of one) its labels.
type ColSchema struct {
	Name     string
	Code     string
	Nullable bool
	Enum     []string
}

// DescribeOptions configures Describe.
type DescribeOptions struct {
	// Identity is passed to publication builders, for those that refuse
	// anonymous callers. Nil describes them as anonymous.
	Identity *Identity
}

// Describe reports a's methods and publications. Method args and results
// come from their Go types. A publication's row comes from PubOptions.Row
// when set; otherwise its builder is called with zero-value args and
// opts.Identity, and the query is run with LIMIT 0 to read the result
// columns. Postgres reports no nullability for result columns, so a column
// is non-null only when the publication reads a single table and the
// column is a NOT NULL column of it (an outer self-join can still make it
// null; use Row then). Every other column is nullable.
//
// Describe needs the database the App uses; it changes nothing in it.
func Describe(ctx context.Context, a *App, opts DescribeOptions) (*Schema, error) {
	s := &Schema{}
	for _, name := range slices.Sorted(maps.Keys(a.methods)) {
		m := a.methods[name]
		s.Methods = append(s.Methods, MethodSchema{Name: name, Args: argsTS(m.argType), Result: jsonTS(m.resType)})
	}
	e := newEngine(a) // its own type cache; the running engine's is not shared
	for _, name := range slices.Sorted(maps.Keys(a.pubs)) {
		p := a.pubs[name]
		ps := PubSchema{Name: name, Args: argsTS(p.argType)}
		var err error
		if p.opts.Row != nil {
			ps.Cols, err = rowCols(reflect.TypeOf(p.opts.Row))
		} else {
			ps.Cols, err = e.describeQuery(ctx, p, opts.Identity)
		}
		if err != nil {
			return nil, fmt.Errorf("publication %s: %w", name, err)
		}
		s.Publications = append(s.Publications, ps)
	}
	return s, nil
}

func (e *engine) describeQuery(ctx context.Context, p *publication, ident *Identity) (_ []ColSchema, err error) {
	// Query.Args encoding (driver.Valuer, pgx encoders) and pool hooks run
	// user code in the queries below.
	defer recoverErr(&err)
	q, err := func() (q Query, err error) {
		defer recoverErr(&err)
		return p.build(&SubCtx{Context: ctx, Identity: ident}, json.RawMessage("null"))
	}()
	if err != nil {
		return nil, fmt.Errorf("build query with zero-value args: %w (set PubOptions.Row or DescribeOptions.Identity)", err)
	}
	tx, err := e.a.db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.Background())
	sql := "SELECT * FROM (" + strings.TrimRight(strings.TrimSpace(q.SQL), "; \t\r\n") + "\n) q LIMIT 0"
	rows, err := tx.Query(ctx, sql, q.Args...)
	if err != nil {
		return nil, fmt.Errorf("describe query: %w (set PubOptions.Row if it needs real args)", err)
	}
	fds := slices.Clone(rows.FieldDescriptions())
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("describe query: %w (set PubOptions.Row if it needs real args)", err)
	}
	m := tx.Conn().TypeMap()
	if err := e.resolveTypes(ctx, m, fds); err != nil {
		return nil, err
	}
	notNull, err := notNullCols(ctx, tx, p, sql, q.Args, fds)
	if err != nil {
		return nil, err
	}
	// The client keeps the last of several columns with one name.
	last := map[string]int{}
	for i, fd := range fds {
		last[fd.Name] = i
	}
	var cols []ColSchema
	enums := map[int]uint32{} // index in cols: type OID
	for i, fd := range fds {
		if last[fd.Name] != i {
			continue
		}
		c := ColSchema{
			Name: fd.Name,
			Code: e.colCode(m, fd.DataTypeOID),
			// A row with a null key fails the query, so a key is never null.
			Nullable: !slices.Contains(p.opts.Key, fd.Name) && !notNull[[2]uint32{fd.TableOID, uint32(fd.TableAttributeNumber)}],
		}
		if strings.TrimPrefix(c.Code, "[]") == "enum" {
			enums[len(cols)] = fd.DataTypeOID
		}
		cols = append(cols, c)
	}
	if len(enums) > 0 {
		labels, err := enumLabels(ctx, tx, slices.Collect(maps.Values(enums)))
		if err != nil {
			return nil, err
		}
		for i, oid := range enums {
			cols[i].Enum = labels[oid]
		}
	}
	return cols, nil
}

// notNullCols returns the (table OID, attnum) pairs that are NOT NULL
// columns of p's table, when p reads exactly one table and sql's plan has
// no outer join and no grouping sets. Either can put null in such a column:
// a column of a multi-table publication, or of an outer join against a
// function or VALUES, may come from the null side, and ROLLUP and its kin
// null out grouped columns.
func notNullCols(ctx context.Context, tx pgx.Tx, p *publication, sql string, args []any, fds []pgconn.FieldDescription) (map[[2]uint32]bool, error) {
	out := map[[2]uint32]bool{}
	if len(p.tables) != 1 {
		return out, nil
	}
	var plan []any
	if err := tx.QueryRow(ctx, "EXPLAIN (VERBOSE, FORMAT JSON) "+sql, args...).Scan(&plan); err != nil {
		return nil, fmt.Errorf("explain query: %w", err)
	}
	if mayAddNulls(plan) {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT attrelid, attnum FROM pg_attribute
		WHERE attrelid = $1::regclass AND attnum > 0 AND attnotnull AND NOT attisdropped`, p.tables[0])
	if err != nil {
		return nil, fmt.Errorf("look up NOT NULL columns of %s: %w", p.tables[0], err)
	}
	defer rows.Close()
	for rows.Next() {
		var rel uint32
		var num int16
		if err := rows.Scan(&rel, &num); err != nil {
			return nil, err
		}
		out[[2]uint32{rel, uint32(num)}] = true
	}
	return out, rows.Err()
}

// enumLabels maps each enum type OID, or array of enum (or of a domain over
// one) OID, to the enum's labels in sort order.
func enumLabels(ctx context.Context, tx pgx.Tx, oids []uint32) (map[uint32][]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT t.oid, array_agg(e.enumlabel::text ORDER BY e.enumsortorder)
		FROM pg_type t
		JOIN pg_type el ON el.oid = CASE WHEN t.typtype = 'e' THEN t.oid ELSE t.typelem END
		JOIN pg_enum e ON e.enumtypid = CASE WHEN el.typtype = 'd' THEN el.typbasetype ELSE el.oid END
		WHERE t.oid = ANY($1)
		GROUP BY t.oid`, oids)
	if err != nil {
		return nil, fmt.Errorf("look up enum labels: %w", err)
	}
	defer rows.Close()
	out := map[uint32][]string{}
	for rows.Next() {
		var oid uint32
		var labels []string
		if err := rows.Scan(&oid, &labels); err != nil {
			return nil, err
		}
		out[oid] = labels
	}
	return out, rows.Err()
}

// mayAddNulls reports whether an EXPLAIN (FORMAT JSON) plan has a node
// that can produce null in a NOT NULL column: an outer join, or an
// aggregate over grouping sets.
func mayAddNulls(v any) bool {
	switch v := v.(type) {
	case []any:
		return slices.ContainsFunc(v, mayAddNulls)
	case map[string]any:
		if jt, ok := v["Join Type"].(string); ok && jt != "Inner" && jt != "Semi" {
			return true
		}
		if _, ok := v["Grouping Sets"]; ok {
			return true
		}
		for _, c := range v {
			if mayAddNulls(c) {
				return true
			}
		}
	}
	return false
}
